package gtm

import (
	"fmt"
	"github.com/globalsign/mgo"
	"github.com/globalsign/mgo/bson"
	"github.com/pkg/errors"
	"github.com/serialx/hashring"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type OrderingGuarantee int

const (
	Oplog     OrderingGuarantee = iota // ops sent in oplog order (strong ordering)
	Namespace                          // ops sent in oplog order within a namespace
	Document                           // ops sent in oplog order for a single document
)

type QuerySource int

const (
	OplogQuerySource QuerySource = iota
	DirectQuerySource
)

type Options struct {
	After               TimestampGenerator
	Filter              OpFilter
	NamespaceFilter     OpFilter
	OpLogDatabaseName   *string
	OpLogCollectionName *string
	CursorTimeout       *string
	ChannelSize         int
	BufferSize          int
	BufferDuration      time.Duration
	EOFDuration         time.Duration
	Ordering            OrderingGuarantee
	WorkerCount         int
	UpdateDataAsDelta   bool
	DirectReadNs        []string
	DirectReadFilter    OpFilter
	DirectReadBatchSize int
	DirectReadCursors   int
	Unmarshal           DataUnmarshaller
	Log                 *log.Logger
}

type Op struct {
	Id        interface{}            `json:"_id"`
	Operation string                 `json:"operation"`
	Namespace string                 `json:"namespace"`
	Data      map[string]interface{} `json:"data,omitempty"`
	Timestamp bson.MongoTimestamp    `json:"timestamp"`
	Source    QuerySource            `json:"source"`
	Doc       interface{}            `json:"doc,omitempty"`
}

type OpLog struct {
	Timestamp    bson.MongoTimestamp "ts"
	HistoryID    int64               "h"
	MongoVersion int                 "v"
	Operation    string              "op"
	Namespace    string              "ns"
	Doc          *bson.Raw           "o"
	Update       *bson.Raw           "o2"
}

type CursorInfo struct {
	Firstbatch []bson.Raw "firstBatch"
	Namespace  string     "ns"
	Id         int64      "id"
}

type Cursor struct {
	Info CursorInfo "cursor"
	Ok   bool       "ok"
}

type PCollectionScanResult struct {
	Cursors []Cursor "cursors"
	Ok      int      "ok"
}

type PCollectionScan struct {
	Namespace  string "parallelCollectionScan"
	Numcursors int    "numCursors"
}

type Doc struct {
	Id interface{} "_id"
}

type OpChan chan *Op

type OpLogEntry map[string]interface{}

type OpFilter func(*Op) bool

type ShardInsertHandler func(*ShardInfo) (*mgo.Session, error)

type TimestampGenerator func(*mgo.Session, *Options) bson.MongoTimestamp

type DataUnmarshaller func(namespace string, raw *bson.Raw) (interface{}, error)

type OpBuf struct {
	Entries        []*Op
	BufferSize     int
	BufferDuration time.Duration
	FlushTicker    *time.Ticker
}

type OpCtx struct {
	lock         *sync.Mutex
	OpC          OpChan
	ErrC         chan error
	DirectReadWg *sync.WaitGroup
	stopC        chan bool
	allWg        *sync.WaitGroup
	seekC        chan bson.MongoTimestamp
	pauseC       chan bool
	resumeC      chan bool
	paused       bool
	stopped      bool
	log          *log.Logger
}

type OpCtxMulti struct {
	lock         *sync.Mutex
	contexts     []*OpCtx
	OpC          OpChan
	ErrC         chan error
	DirectReadWg *sync.WaitGroup
	stopC        chan bool
	allWg        *sync.WaitGroup
	seekC        chan bson.MongoTimestamp
	pauseC       chan bool
	resumeC      chan bool
	paused       bool
	stopped      bool
	log          *log.Logger
}

type ShardInfo struct {
	hostname string
}

type BuildInfo struct {
	version []int
	major   int
	minor   int
	patch   int
}

type N struct {
	database   string
	collection string
}

func (b *BuildInfo) build() {
	parts := len(b.version)
	if parts > 0 {
		b.major = b.version[0]
	}
	if parts > 1 {
		b.minor = b.version[1]
	}
	if parts > 2 {
		b.patch = b.version[2]
	}
}

func (n *N) parse(ns string) (err error) {
	parts := strings.SplitN(ns, ".", 2)
	if len(parts) != 2 {
		err = fmt.Errorf("Invalid ns: %s :expecting db.collection", ns)
	} else {
		n.database = parts[0]
		n.collection = parts[1]
	}
	return
}

func (shard *ShardInfo) GetURL() string {
	hostParts := strings.SplitN(shard.hostname, "/", 2)
	if len(hostParts) == 2 {
		return hostParts[1] + "?replicaSet=" + hostParts[0]
	} else {
		return hostParts[0]
	}
}

func (ctx *OpCtx) waitForConnection(wg *sync.WaitGroup, session *mgo.Session, options *Options) {
	defer wg.Done()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.stopC:
			return
		case <-t.C:
			s := session.Copy()
			if err := s.Ping(); err == nil {
				s.Close()
				return
			}
			s.Close()
		}
	}
}

func (ctx *OpCtx) isStopped() bool {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	return ctx.stopped
}

func (ctx *OpCtx) Since(ts bson.MongoTimestamp) {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	ctx.seekC <- ts
}

func (ctx *OpCtx) Pause() {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if !ctx.paused {
		ctx.paused = true
		ctx.pauseC <- true
	}
}

func (ctx *OpCtx) Resume() {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if ctx.paused {
		ctx.paused = false
		ctx.resumeC <- true
	}
}

func (ctx *OpCtx) Stop() {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if !ctx.stopped {
		ctx.stopped = true
		close(ctx.stopC)
		ctx.allWg.Wait()
	}
}

func (ctx *OpCtxMulti) Since(ts bson.MongoTimestamp) {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	for _, child := range ctx.contexts {
		child.Since(ts)
	}
}

func (ctx *OpCtxMulti) Pause() {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if !ctx.paused {
		ctx.paused = true
		ctx.pauseC <- true
		for _, child := range ctx.contexts {
			child.Pause()
		}
	}
}

func (ctx *OpCtxMulti) Resume() {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if ctx.paused {
		ctx.paused = false
		ctx.resumeC <- true
		for _, child := range ctx.contexts {
			child.Resume()
		}
	}
}

func (ctx *OpCtxMulti) Stop() {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if !ctx.stopped {
		ctx.stopped = true
		close(ctx.stopC)
		for _, child := range ctx.contexts {
			go child.Stop()
		}
		ctx.allWg.Wait()
	}
}

func tailShards(multi *OpCtxMulti, ctx *OpCtx, options *Options, handler ShardInsertHandler) {
	defer multi.allWg.Done()
	if options == nil {
		options = DefaultOptions()
	} else {
		options.SetDefaults()
	}
	for {
		select {
		case <-multi.stopC:
			return
		case <-multi.pauseC:
			<-multi.resumeC
			select {
			case <-multi.stopC:
				return
			}
		case err := <-ctx.ErrC:
			multi.ErrC <- err
		case op := <-ctx.OpC:
			// new shard detected
			shardInfo := &ShardInfo{
				hostname: op.Data["host"].(string),
			}
			shardSession, err := handler(shardInfo)
			if err != nil {
				multi.ErrC <- errors.Wrap(err, "Error calling shard handler")
				continue
			}
			shardCtx := Start(shardSession, options)
			multi.lock.Lock()
			multi.contexts = append(multi.contexts, shardCtx)
			multi.DirectReadWg.Add(1)
			go func() {
				defer multi.DirectReadWg.Done()
				shardCtx.DirectReadWg.Wait()
			}()
			multi.allWg.Add(1)
			go func() {
				defer multi.allWg.Done()
				shardCtx.allWg.Wait()
			}()
			go func(c OpChan) {
				for op := range c {
					multi.OpC <- op
				}
			}(shardCtx.OpC)
			go func(c chan error) {
				for err := range c {
					multi.ErrC <- err
				}
			}(shardCtx.ErrC)
			multi.lock.Unlock()
		}
	}
}

func (ctx *OpCtxMulti) AddShardListener(
	configSession *mgo.Session, shardOptions *Options, handler ShardInsertHandler) {
	opts := DefaultOptions()
	opts.NamespaceFilter = func(op *Op) bool {
		return op.Namespace == "config.shards" && op.IsInsert()
	}
	configCtx := Start(configSession, opts)
	ctx.allWg.Add(1)
	go tailShards(ctx, configCtx, shardOptions, handler)
}

func ChainOpFilters(filters ...OpFilter) OpFilter {
	return func(op *Op) bool {
		for _, filter := range filters {
			if filter(op) == false {
				return false
			}
		}
		return true
	}
}

func (this *Op) IsDrop() bool {
	if _, drop := this.IsDropDatabase(); drop {
		return true
	}
	if _, drop := this.IsDropCollection(); drop {
		return true
	}
	return false
}

func (this *Op) IsDropCollection() (string, bool) {
	if this.IsCommand() {
		if this.Data != nil {
			if val, ok := this.Data["drop"]; ok {
				return val.(string), true
			}
		}
	}
	return "", false
}

func (this *Op) IsDropDatabase() (string, bool) {
	if this.IsCommand() {
		if this.Data != nil {
			if _, ok := this.Data["dropDatabase"]; ok {
				return this.GetDatabase(), true
			}
		}
	}
	return "", false
}

func (this *Op) IsCommand() bool {
	return this.Operation == "c"
}

func (this *Op) IsInsert() bool {
	return this.Operation == "i"
}

func (this *Op) IsUpdate() bool {
	return this.Operation == "u"
}

func (this *Op) IsDelete() bool {
	return this.Operation == "d"
}

func (this *Op) IsSourceOplog() bool {
	return this.Source == OplogQuerySource
}

func (this *Op) IsSourceDirect() bool {
	return this.Source == DirectQuerySource
}

func (this *Op) ParseNamespace() []string {
	return strings.SplitN(this.Namespace, ".", 2)
}

func (this *Op) GetDatabase() string {
	return this.ParseNamespace()[0]
}

func (this *Op) GetCollection() string {
	if _, drop := this.IsDropDatabase(); drop {
		return ""
	} else if col, drop := this.IsDropCollection(); drop {
		return col
	} else {
		return this.ParseNamespace()[1]
	}
}

func (this *OpBuf) Append(op *Op) {
	this.Entries = append(this.Entries, op)
}

func (this *OpBuf) IsFull() bool {
	return len(this.Entries) >= this.BufferSize
}

func (this *OpBuf) Flush(session *mgo.Session, ctx *OpCtx, options *Options) {
	if len(this.Entries) == 0 {
		return
	}
	ns := make(map[string][]interface{})
	byId := make(map[interface{}][]*Op)
	for _, op := range this.Entries {
		if op.IsUpdate() && op.Doc == nil {
			idKey := fmt.Sprintf("%s.%v", op.Namespace, op.Id)
			ns[op.Namespace] = append(ns[op.Namespace], op.Id)
			byId[idKey] = append(byId[idKey], op)
		}
	}
Retry:
	for n, opIds := range ns {
		var parts = strings.SplitN(n, ".", 2)
		var results []*bson.Raw
		db, col := parts[0], parts[1]
		sel := bson.M{"_id": bson.M{"$in": opIds}}
		collection := session.DB(db).C(col)
		err := collection.Find(sel).All(&results)
		if err == nil {
			for _, result := range results {
				var doc Doc
				result.Unmarshal(&doc)
				resultId := fmt.Sprintf("%s.%v", n, doc.Id)
				if ops, ok := byId[resultId]; ok {
					for _, o := range ops {
						if u, err := options.Unmarshal(o.Namespace, result); err == nil {
							o.processData(u)
						} else {
							ctx.ErrC <- err
						}
					}
				}
			}
		} else {
			ctx.ErrC <- errors.Wrap(err, "Error finding documents to associate with ops")
			var wg sync.WaitGroup
			wg.Add(1)
			go ctx.waitForConnection(&wg, session, options)
			wg.Wait()
			if ctx.isStopped() {
				this.Entries = nil
				return
			}
			session.Refresh()
			break Retry
		}
	}
	for _, op := range this.Entries {
		if op.matchesFilter(options) {
			ctx.OpC <- op
		}
	}
	this.Entries = nil
}

func UpdateIsReplace(entry map[string]interface{}) bool {
	if _, ok := entry["$set"]; ok {
		return false
	} else if _, ok := entry["$unset"]; ok {
		return false
	} else {
		return true
	}
}

func (this *Op) shouldParse() bool {
	return this.IsInsert() || this.IsDelete() || this.IsUpdate() || this.IsCommand()
}

func (this *Op) matchesNsFilter(options *Options) bool {
	return options.NamespaceFilter == nil || options.NamespaceFilter(this)
}

func (this *Op) matchesFilter(options *Options) bool {
	return options.Filter == nil || options.Filter(this)
}

func (this *Op) matchesDirectFilter(options *Options) bool {
	return options.DirectReadFilter == nil || options.DirectReadFilter(this)
}

func (this *Op) processData(data interface{}) {
	if data != nil {
		this.Doc = data
		if m, ok := data.(map[string]interface{}); ok {
			this.Data = m
		}
	}
}

func (this *Op) ParseLogEntry(entry *OpLog, options *Options) (include bool, err error) {
	var rawField *bson.Raw
	var u interface{}
	this.Operation = entry.Operation
	this.Timestamp = entry.Timestamp
	this.Namespace = entry.Namespace
	if this.shouldParse() {
		if this.IsCommand() {
			var objectField map[string]interface{}
			rawField = entry.Doc
			err = rawField.Unmarshal(&objectField)
			this.processData(objectField)
		}
		if this.matchesNsFilter(options) {
			if this.IsInsert() || this.IsDelete() || this.IsUpdate() {
				if this.IsUpdate() {
					rawField = entry.Update
				} else {
					rawField = entry.Doc
				}
				var doc Doc
				rawField.Unmarshal(&doc)
				this.Id = doc.Id
				if this.IsInsert() {
					if u, err = options.Unmarshal(this.Namespace, rawField); err == nil {
						this.processData(u)
					}
				} else if this.IsUpdate() {
					var changeField map[string]interface{}
					rawField = entry.Doc
					rawField.Unmarshal(&changeField)
					if options.UpdateDataAsDelta || UpdateIsReplace(changeField) {
						if u, err = options.Unmarshal(this.Namespace, rawField); err == nil {
							this.processData(u)
						}
					}
				}
				include = true
			} else if this.IsCommand() {
				include = this.IsDrop()
			}
		}
	}
	return
}

func OpLogCollectionName(session *mgo.Session, options *Options) string {
	localDB := session.DB(*options.OpLogDatabaseName)
	col_names, err := localDB.CollectionNames()
	if err == nil {
		var col_name *string = nil
		for _, name := range col_names {
			if strings.HasPrefix(name, "oplog.") {
				col_name = &name
				break
			}
		}
		if col_name == nil {
			msg := fmt.Sprintf(`
				Unable to find oplog collection 
				in database %v`, *options.OpLogDatabaseName)
			panic(msg)
		} else {
			return *col_name
		}
	} else {
		msg := fmt.Sprintf(`Unable to get collection names 
		for database %v: %s`, *options.OpLogDatabaseName, err)
		panic(msg)
	}
}

func OpLogCollection(session *mgo.Session, options *Options) *mgo.Collection {
	localDB := session.DB(*options.OpLogDatabaseName)
	return localDB.C(*options.OpLogCollectionName)
}

func ParseTimestamp(timestamp bson.MongoTimestamp) (int32, int32) {
	ordinal := (timestamp << 32) >> 32
	ts := (timestamp >> 32)
	return int32(ts), int32(ordinal)
}

func LastOpTimestamp(session *mgo.Session, options *Options) bson.MongoTimestamp {
	var opLog OpLog
	collection := OpLogCollection(session, options)
	collection.Find(nil).Sort("-$natural").One(&opLog)
	return opLog.Timestamp
}

func GetOpLogQuery(session *mgo.Session, after bson.MongoTimestamp, options *Options) *mgo.Query {
	query := bson.M{"ts": bson.M{"$gt": after}, "fromMigrate": bson.M{"$exists": false}}
	collection := OpLogCollection(session, options)
	return collection.Find(query).LogReplay().Sort("$natural")
}

func TailOps(ctx *OpCtx, session *mgo.Session, channels []OpChan, options *Options) error {
	defer ctx.allWg.Done()
	s := session.Copy()
	defer s.Close()
	options.Fill(s)
	duration, err := time.ParseDuration(*options.CursorTimeout)
	if err != nil {
		panic(fmt.Sprintf("Invalid value <%s> for CursorTimeout", *options.CursorTimeout))
	}
	currTimestamp := options.After(s, options)
	iter := GetOpLogQuery(s, currTimestamp, options).Tail(duration)
	for {
		var entry OpLog
	Seek:
		for iter.Next(&entry) {
			op := &Op{
				Id:        "",
				Operation: "",
				Namespace: "",
				Data:      nil,
				Timestamp: bson.MongoTimestamp(0),
				Source:    OplogQuerySource,
			}
			ok, err := op.ParseLogEntry(&entry, options)
			if err == nil {
				if ok && op.matchesFilter(options) {
					if options.UpdateDataAsDelta {
						ctx.OpC <- op
					} else {
						// broadcast to fetch channels
						for _, channel := range channels {
							channel <- op
						}
					}
				}
			} else {
				ctx.ErrC <- err
			}
			select {
			case <-ctx.stopC:
				return nil
			case ts := <-ctx.seekC:
				currTimestamp = ts
				break Seek
			case <-ctx.pauseC:
				<-ctx.resumeC
				select {
				case <-ctx.stopC:
					return nil
				case ts := <-ctx.seekC:
					currTimestamp = ts
					break Seek
				default:
					currTimestamp = op.Timestamp
				}
			default:
				currTimestamp = op.Timestamp
			}
		}
		if err = iter.Close(); err != nil {
			ctx.ErrC <- errors.Wrap(err, "Error tailing oplog entries")
			var wg sync.WaitGroup
			wg.Add(1)
			go ctx.waitForConnection(&wg, s, options)
			wg.Wait()
			if ctx.isStopped() {
				return nil
			}
			s.Refresh()
			iter = GetOpLogQuery(s, currTimestamp, options).Tail(duration)
			continue
		}
		if iter.Timeout() {
			select {
			case <-ctx.stopC:
				return nil
			case ts := <-ctx.seekC:
				currTimestamp = ts
			case <-ctx.pauseC:
				<-ctx.resumeC
				select {
				case ts := <-ctx.seekC:
					currTimestamp = ts
				default:
					continue
				}
			default:
				continue
			}
		}
		iter = GetOpLogQuery(s, currTimestamp, options).Tail(duration)
	}
	return nil
}

func SupportsCollectionScan(session *mgo.Session) (supports bool, err error) {
	var buildInfo *BuildInfo
	if buildInfo, err = VersionInfo(session); err == nil {
		if buildInfo.major > 2 {
			supports = true
		} else if buildInfo.major == 2 && buildInfo.minor >= 6 {
			supports = true
		}
	}
	return
}

func DirectReadCollectionScan(ctx *OpCtx, session *mgo.Session, ns string, options *Options) (err error) {
	defer ctx.allWg.Done()
	defer ctx.DirectReadWg.Done()
	n := &N{}
	if err = n.parse(ns); err != nil {
		ctx.ErrC <- errors.Wrap(err, "Error parsing direct read namespace")
		return
	}
	scan := PCollectionScan{
		Namespace:  n.collection,
		Numcursors: options.DirectReadCursors,
	}
	var result PCollectionScanResult
	s := session.Copy()
	err = s.DB(n.database).Run(scan, &result)
	if err != nil || result.Ok == 0 {
		defer s.Close()
		msg := fmt.Sprintf("Parallel collection scan of %s failed", ns)
		ctx.ErrC <- errors.Wrap(err, msg)
		ctx.log.Println("Reverting to single-threaded collection read")
		ctx.allWg.Add(1)
		ctx.DirectReadWg.Add(1)
		go DirectRead(ctx, session, ns, options)
		return
	}
	if len(result.Cursors) > 1 {
		for _, cursor := range result.Cursors {
			ctx.allWg.Add(1)
			ctx.DirectReadWg.Add(1)
			go DirectReadCursor(ctx, s, ns, options, cursor.Info)
		}
	} else {
		defer s.Close()
		if scan.Numcursors > 1 {
			ctx.log.Println("Only 1 cursor available for collection scan in this storage engine")
		}
		ctx.log.Println("Reverting to single-threaded collection read")
		ctx.allWg.Add(1)
		ctx.DirectReadWg.Add(1)
		go DirectRead(ctx, session, ns, options)
	}
	return
}

func DirectReadCursor(ctx *OpCtx, s *mgo.Session, ns string, options *Options, cursor CursorInfo) (err error) {
	defer ctx.allWg.Done()
	defer ctx.DirectReadWg.Done()
	n := &N{}
	if err = n.parse(ns); err != nil {
		ctx.ErrC <- errors.Wrap(err, "Error parsing direct read namespace")
		return
	}
	c := s.DB(n.database).C(n.collection)
	iter := c.NewIter(nil, cursor.Firstbatch, cursor.Id, nil)
	for {
		foundResults := false
		var result = &bson.Raw{}
		for iter.Next(result) {
			foundResults = true
			t := time.Now().UTC().Unix()
			var doc Doc
			result.Unmarshal(&doc)
			op := &Op{
				Id:        doc.Id,
				Operation: "i",
				Namespace: ns,
				Source:    DirectQuerySource,
				Timestamp: bson.MongoTimestamp(t << 32),
			}
			if u, err := options.Unmarshal(ns, result); err == nil {
				op.processData(u)
				if op.matchesDirectFilter(options) {
					ctx.OpC <- op
				}
			} else {
				ctx.ErrC <- err
			}
			result = &bson.Raw{}
			select {
			case <-ctx.stopC:
				return
			default:
				continue
			}
		}
		if err = iter.Close(); err != nil {
			ctx.ErrC <- errors.Wrap(err, "Error performing direct reads of collections")
			var wg sync.WaitGroup
			wg.Add(1)
			go ctx.waitForConnection(&wg, s, options)
			wg.Wait()
			if ctx.isStopped() {
				return
			}
			s.Refresh()
			continue
		} else if !foundResults {
			break
		}
	}
	return
}

func DirectRead(ctx *OpCtx, session *mgo.Session, ns string, options *Options) (err error) {
	defer ctx.allWg.Done()
	defer ctx.DirectReadWg.Done()
	s := session.Copy()
	defer s.Close()
	n := &N{}
	if err = n.parse(ns); err != nil {
		ctx.ErrC <- errors.Wrap(err, "Error parsing direct read namespace")
		return
	}
	c := s.DB(n.database).C(n.collection)
	var sel bson.M = nil
	for {
		foundResults := false
		q := c.Find(sel).Sort("_id").Hint("_id").Batch(options.DirectReadBatchSize)
		iter := q.Iter()
		var result = &bson.Raw{}
		for iter.Next(result) {
			foundResults = true
			var doc Doc
			result.Unmarshal(&doc)
			sel = bson.M{"_id": bson.M{"$gt": doc.Id}}
			t := time.Now().UTC().Unix()
			op := &Op{
				Id:        doc.Id,
				Operation: "i",
				Namespace: ns,
				Source:    DirectQuerySource,
				Timestamp: bson.MongoTimestamp(t << 32),
			}
			if u, err := options.Unmarshal(ns, result); err == nil {
				op.processData(u)
				if op.matchesDirectFilter(options) {
					ctx.OpC <- op
				}
			} else {
				ctx.ErrC <- err
			}
			result = &bson.Raw{}
			select {
			case <-ctx.stopC:
				return
			default:
				continue
			}
		}
		if err = iter.Close(); err != nil {
			ctx.ErrC <- errors.Wrap(err, "Error performing direct reads of collections")
			var wg sync.WaitGroup
			wg.Add(1)
			go ctx.waitForConnection(&wg, s, options)
			wg.Wait()
			if ctx.isStopped() {
				return
			}
			s.Refresh()
			continue
		} else if !foundResults {
			break
		}
	}
	return
}

func FetchDocuments(ctx *OpCtx, session *mgo.Session, filter OpFilter, buf *OpBuf, inOp OpChan, options *Options) error {
	defer ctx.allWg.Done()
	s := session.Copy()
	defer s.Close()
	for {
		select {
		case <-ctx.stopC:
			return nil
		case <-buf.FlushTicker.C:
			buf.Flush(s, ctx, options)
		case op := <-inOp:
			if filter(op) {
				buf.Append(op)
				if buf.IsFull() {
					buf.Flush(s, ctx, options)
					buf.FlushTicker.Stop()
					buf.FlushTicker = time.NewTicker(buf.BufferDuration)
				}
			}
		}
	}
	return nil
}

func OpFilterForOrdering(ordering OrderingGuarantee, workers []string, worker string) OpFilter {
	switch ordering {
	case Document:
		ring := hashring.New(workers)
		return func(op *Op) bool {
			var key string
			if op.Id != nil {
				key = fmt.Sprintf("%v", op.Id)
			} else {
				key = op.Namespace
			}
			if who, ok := ring.GetNode(key); ok {
				return who == worker
			} else {
				return false
			}
		}
	case Namespace:
		ring := hashring.New(workers)
		return func(op *Op) bool {
			if who, ok := ring.GetNode(op.Namespace); ok {
				return who == worker
			} else {
				return false
			}
		}
	default:
		return func(op *Op) bool {
			return true
		}
	}
}

func DefaultOptions() *Options {
	return &Options{
		After:               nil,
		Filter:              nil,
		NamespaceFilter:     nil,
		OpLogDatabaseName:   nil,
		OpLogCollectionName: nil,
		CursorTimeout:       nil,
		ChannelSize:         512,
		BufferSize:          50,
		BufferDuration:      time.Duration(750) * time.Millisecond,
		EOFDuration:         time.Duration(5) * time.Second,
		Ordering:            Oplog,
		WorkerCount:         1,
		UpdateDataAsDelta:   false,
		DirectReadNs:        []string{},
		DirectReadFilter:    nil,
		DirectReadBatchSize: 500,
		DirectReadCursors:   10,
		Unmarshal:           defaultUnmarshaller,
		Log:                 log.New(os.Stdout, "INFO ", log.Flags()),
	}
}

func (this *Options) Fill(session *mgo.Session) {
	if this.After == nil {
		this.After = LastOpTimestamp
	}
	if this.OpLogDatabaseName == nil {
		defaultOpLogDatabaseName := "local"
		this.OpLogDatabaseName = &defaultOpLogDatabaseName
	}
	if this.OpLogCollectionName == nil {
		defaultOpLogCollectionName := OpLogCollectionName(session, this)
		this.OpLogCollectionName = &defaultOpLogCollectionName
	}
	if this.CursorTimeout == nil {
		defaultCursorTimeout := "100s"
		this.CursorTimeout = &defaultCursorTimeout
	}
}

func defaultUnmarshaller(namespace string, raw *bson.Raw) (interface{}, error) {
	var m map[string]interface{}
	if err := raw.Unmarshal(&m); err == nil {
		return m, nil
	} else {
		return nil, err
	}
}

func (this *Options) SetDefaults() {
	defaultOpts := DefaultOptions()
	if this.ChannelSize < 1 {
		this.ChannelSize = defaultOpts.ChannelSize
	}
	if this.BufferSize < 1 {
		this.BufferSize = defaultOpts.BufferSize
	}
	if this.BufferDuration == 0 {
		this.BufferDuration = defaultOpts.BufferDuration
	}
	if this.Ordering == Oplog {
		this.WorkerCount = 1
	}
	if this.WorkerCount < 1 {
		this.WorkerCount = 1
	}
	if this.UpdateDataAsDelta {
		this.Ordering = Oplog
		this.WorkerCount = 0
	}
	if this.DirectReadBatchSize < 1 {
		this.DirectReadBatchSize = defaultOpts.DirectReadBatchSize
	}
	if this.DirectReadCursors < 1 {
		this.DirectReadCursors = defaultOpts.DirectReadCursors
	}
	if this.EOFDuration == 0 {
		this.EOFDuration = defaultOpts.EOFDuration
	}
	if this.Unmarshal == nil {
		this.Unmarshal = defaultOpts.Unmarshal
	}
	if this.Log == nil {
		this.Log = defaultOpts.Log
	}
}

func Tail(session *mgo.Session, options *Options) (OpChan, chan error) {
	ctx := Start(session, options)
	return ctx.OpC, ctx.ErrC
}

func GetShards(session *mgo.Session) (shardInfos []*ShardInfo) {
	// use this for sharded databases to get the shard hosts
	// use the hostnames to create multiple sessions for a call to StartMulti
	col := session.DB("config").C("shards")
	var shards []map[string]interface{}
	col.Find(nil).All(&shards)
	for _, shard := range shards {
		host := shard["host"].(string)
		shardInfo := &ShardInfo{
			hostname: host,
		}
		shardInfos = append(shardInfos, shardInfo)
	}
	return
}

func VersionInfo(session *mgo.Session) (buildInfo *BuildInfo, err error) {
	if info, err := session.BuildInfo(); err == nil {
		buildInfo = &BuildInfo{
			version: info.VersionArray,
		}
		buildInfo.build()
	}
	return
}

func StartMulti(sessions []*mgo.Session, options *Options) *OpCtxMulti {
	if options == nil {
		options = DefaultOptions()
	} else {
		options.SetDefaults()
	}

	stopC := make(chan bool, 1)
	errC := make(chan error, options.ChannelSize)
	opC := make(OpChan, options.ChannelSize)

	var directReadWg sync.WaitGroup
	var allWg sync.WaitGroup
	var seekC = make(chan bson.MongoTimestamp, 1)
	var pauseC = make(chan bool, 1)
	var resumeC = make(chan bool, 1)

	ctxMulti := &OpCtxMulti{
		lock:         &sync.Mutex{},
		OpC:          opC,
		ErrC:         errC,
		DirectReadWg: &directReadWg,
		stopC:        stopC,
		allWg:        &allWg,
		pauseC:       pauseC,
		resumeC:      resumeC,
		seekC:        seekC,
		log:          options.Log,
	}

	ctxMulti.lock.Lock()
	defer ctxMulti.lock.Unlock()

	for _, session := range sessions {
		ctx := Start(session, options)
		ctxMulti.contexts = append(ctxMulti.contexts, ctx)
		directReadWg.Add(1)
		go func() {
			defer directReadWg.Done()
			ctx.DirectReadWg.Wait()
		}()
		allWg.Add(1)
		go func() {
			defer allWg.Done()
			ctx.allWg.Wait()
		}()
		go func(c OpChan) {
			for op := range c {
				opC <- op
			}
		}(ctx.OpC)
		go func(c chan error) {
			for err := range c {
				errC <- err
			}
		}(ctx.ErrC)
	}
	return ctxMulti
}

func Start(session *mgo.Session, options *Options) *OpCtx {
	if options == nil {
		options = DefaultOptions()
	} else {
		options.SetDefaults()
	}

	stopC := make(chan bool)
	errC := make(chan error, options.ChannelSize)
	opC := make(OpChan, options.ChannelSize)

	var inOps []OpChan
	var workerNames []string
	var directReadWg sync.WaitGroup
	var allWg sync.WaitGroup
	var seekC = make(chan bson.MongoTimestamp, 1)
	var pauseC = make(chan bool, 1)
	var resumeC = make(chan bool, 1)

	ctx := &OpCtx{
		lock:         &sync.Mutex{},
		OpC:          opC,
		ErrC:         errC,
		DirectReadWg: &directReadWg,
		stopC:        stopC,
		allWg:        &allWg,
		pauseC:       pauseC,
		resumeC:      resumeC,
		seekC:        seekC,
		log:          options.Log,
	}

	for i := 1; i <= options.WorkerCount; i++ {
		workerNames = append(workerNames, strconv.Itoa(i))
	}

	for i := 1; i <= options.WorkerCount; i++ {
		allWg.Add(1)
		inOp := make(OpChan, options.ChannelSize)
		inOps = append(inOps, inOp)
		buf := &OpBuf{
			BufferSize:     options.BufferSize,
			BufferDuration: options.BufferDuration,
			FlushTicker:    time.NewTicker(options.BufferDuration),
		}
		worker := strconv.Itoa(i)
		filter := OpFilterForOrdering(options.Ordering, workerNames, worker)
		go FetchDocuments(ctx, session, filter, buf, inOp, options)
	}

	var scanOk bool
	var err error
	if len(options.DirectReadNs) > 0 {
		scanOk, err = SupportsCollectionScan(session)
		if err != nil {
			ctx.ErrC <- errors.Wrap(err, "Error determining collection scan support")
		}
		if scanOk {
			ctx.log.Println("Direct read parallel collection scan is ON")
		}
	}

	for _, ns := range options.DirectReadNs {
		directReadWg.Add(1)
		allWg.Add(1)
		if scanOk {
			go DirectReadCollectionScan(ctx, session, ns, options)
		} else {
			go DirectRead(ctx, session, ns, options)
		}
	}

	allWg.Add(1)
	go TailOps(ctx, session, inOps, options)

	return ctx
}
