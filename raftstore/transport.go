package raftstore

import (
	"fmt"
	"io"
	"sync"
	"time"

	etcdraftpb "github.com/coreos/etcd/raft/raftpb"
	"github.com/deepfabric/beehive/metric"
	"github.com/deepfabric/beehive/pb"
	"github.com/deepfabric/beehive/pb/raftpb"
	"github.com/fagongzi/goetty"
	"github.com/fagongzi/util/protoc"
	"github.com/fagongzi/util/task"
)

// Transport raft transport
type Transport interface {
	Start()
	Stop()
	Send(*raftpb.RaftMessage, *etcdraftpb.Message)
}

type transport struct {
	store  *store
	server *goetty.Server
	conns  sync.Map // store id -> goetty.IOSessionPool

	resolverFunc func(id uint64) (string, error)
	addrs        sync.Map // store id -> addr
	addrsRevert  sync.Map // addr -> store id

	raftMsgs []*task.Queue
	raftMask uint64
	snapMsgs []*task.Queue
	snapMask uint64
}

func newTransport(store *store) Transport {
	t := &transport{
		store: store,
		server: goetty.NewServer(store.cfg.RaftAddr,
			goetty.WithServerDecoder(decoder),
			goetty.WithServerEncoder(encoder),
			goetty.WithServerIDGenerator(goetty.NewUUIDV4IdGenerator())),
		raftMsgs: make([]*task.Queue, store.opts.sendRaftMsgWorkerCount, store.opts.sendRaftMsgWorkerCount),
		raftMask: uint64(store.opts.sendRaftMsgWorkerCount - 1),
		snapMsgs: make([]*task.Queue, store.opts.maxConcurrencySnapChunks, store.opts.maxConcurrencySnapChunks),
		snapMask: uint64(store.opts.maxConcurrencySnapChunks - 1),
	}
	t.resolverFunc = t.resolverStoreAddr
	return t
}

func (t *transport) Start() {
	for i := 0; i < t.store.opts.sendRaftMsgWorkerCount; i++ {
		t.raftMsgs[i] = &task.Queue{}
		go t.readyToSendRaft(t.raftMsgs[i])
	}

	for i := 0; i < t.store.opts.maxConcurrencySnapChunks; i++ {
		t.snapMsgs[i] = &task.Queue{}
		go t.readyToSendSnapshots(t.snapMsgs[i])
	}

	go func() {
		err := t.server.Start(t.doConnection)
		if err != nil {
			logger.Fatalf("transport start at %s failed with %+v",
				t.store.cfg.RaftAddr,
				err)
		}
	}()

	<-t.server.Started()
}

func (t *transport) Stop() {
	for _, q := range t.snapMsgs {
		q.Dispose()
	}

	for _, q := range t.raftMsgs {
		q.Dispose()
	}

	t.server.Stop()
	logger.Infof("transfer stopped")
}

func (t *transport) Send(msg *raftpb.RaftMessage, raw *etcdraftpb.Message) {
	storeID := msg.To.StoreID
	if storeID == t.store.meta.meta.ID {
		t.store.handle(msg)
		return
	}

	if raw.Type == etcdraftpb.MsgSnap {
		snapMsg := &raftpb.SnapshotMessage{}
		protoc.MustUnmarshal(snapMsg, raw.Snapshot.Data)
		snapMsg.Header.From = msg.From
		snapMsg.Header.To = msg.To

		t.snapMsgs[t.snapMask&storeID].Put(snapMsg)
	}

	msg.Message = protoc.MustMarshal(raw)
	t.raftMsgs[t.raftMask&storeID].Put(msg)
}

func (t *transport) doConnection(session goetty.IOSession) error {
	remoteIP := session.RemoteIP()

	logger.Infof("%s connected", remoteIP)
	for {
		msg, err := session.Read()
		if err != nil {
			if err == io.EOF {
				logger.Infof("closed by %s", remoteIP)
			} else {
				logger.Warningf("read from %s failed with %+v",
					remoteIP,
					err)
			}

			return err
		}

		t.store.handle(msg)
	}
}

func (t *transport) readyToSendRaft(q *task.Queue) {
	items := make([]interface{}, t.store.opts.sendRaftBatchSize, t.store.opts.sendRaftBatchSize)

	for {
		n, err := q.Get(int64(t.store.opts.sendRaftBatchSize), items)
		if err != nil {
			logger.Infof("send raft worker stopped")
			return
		}

		for i := int64(0); i < n; i++ {
			msg := items[i].(*raftpb.RaftMessage)
			err := t.doSend(msg, msg.To.StoreID)
			t.postSend(msg, err)
		}

		metric.SetRaftMsgQueueMetric(q.Len())
	}
}

func (t *transport) readyToSendSnapshots(q *task.Queue) {
	items := make([]interface{}, t.store.opts.sendRaftBatchSize, t.store.opts.sendRaftBatchSize)

	for {
		n, err := q.Get(int64(t.store.opts.sendRaftBatchSize), items)
		if err != nil {
			logger.Infof("send snapshot worker stopped")
			return
		}

		for i := int64(0); i < n; i++ {
			msg := items[i].(*raftpb.SnapshotMessage)
			id := msg.Header.To.StoreID

			conn, err := t.getConn(id)
			if err != nil {
				logger.Errorf("create conn to %d failed with %+v",
					id,
					err)
				continue
			}

			err = t.doSendSnapshotMessage(msg, conn)
			t.putConn(id, conn)

			if err != nil {
				logger.Errorf("send snap %s failed with %+v",
					msg.String(),
					err)
			}
		}

		metric.SetRaftSnapQueueMetric(q.Len())
	}
}

func (t *transport) doSendSnapshotMessage(msg *raftpb.SnapshotMessage, conn goetty.IOSession) error {
	if t.store.snapshotManager.Register(msg, Sending) {
		defer t.store.snapshotManager.Deregister(msg, Sending)

		logger.Infof("shard %d start send pending snap, epoch=<%s> term=<%d> index=<%d>",
			msg.Header.Shard.ID,
			msg.Header.Shard.Epoch.String(),
			msg.Header.Term,
			msg.Header.Index)

		start := time.Now()
		if !t.store.snapshotManager.Exists(msg) {
			return fmt.Errorf("transport: missing snapshot file, header=<%+v>",
				msg.Header)
		}

		size, err := t.store.snapshotManager.WriteTo(msg, conn)
		if err != nil {
			conn.Close()
			return err
		}

		t.store.sendingSnapCount++
		logger.Infof("shard %d pending snap sent succ, size=<%d>, epoch=<%s> term=<%d> index=<%d>",
			msg.Header.Shard.ID,
			size,
			msg.Header.Shard.Epoch.String(),
			msg.Header.Term,
			msg.Header.Index)

		metric.ObserveSnapshotSendingDuration(start)
	}

	return nil
}

func (t *transport) postSend(msg *raftpb.RaftMessage, err error) {
	if err != nil {
		logger.Errorf("shard %d send msg from %d to %d failed with %+v",
			msg.ShardID,
			msg.From.ID,
			msg.To.ID,
			err)

		if pr := t.store.getPR(msg.ShardID, true); pr != nil {
			value := etcdraftpb.Message{}
			protoc.MustUnmarshal(&value, msg.Message)
			pr.addReport(value)
		}
	}

	pb.ReleaseRaftMessage(msg)
}

func (t *transport) doSend(msg interface{}, to uint64) error {
	conn, err := t.getConn(to)
	if err != nil {
		return err
	}

	err = t.doWrite(msg, conn)
	t.putConn(to, conn)
	return err
}

func (t *transport) doWrite(msg interface{}, conn goetty.IOSession) error {
	err := conn.WriteAndFlush(msg)
	if err != nil {
		conn.Close()
		return err
	}

	return nil
}

func (t *transport) putConn(id uint64, conn goetty.IOSession) {
	if pool, ok := t.conns.Load(id); ok {
		pool.(goetty.IOSessionPool).Put(conn)
	} else {
		conn.Close()
	}
}

func (t *transport) getConn(id uint64) (goetty.IOSession, error) {
	conn, err := t.getConnLocked(id)
	if err != nil {
		return nil, err
	}

	if t.checkConnect(id, conn) {
		return conn, nil
	}

	t.putConn(id, conn)
	return nil, errConnect
}

func (t *transport) getConnLocked(id uint64) (goetty.IOSession, error) {
	if pool, ok := t.conns.Load(id); ok {
		return pool.(goetty.IOSessionPool).Get()
	}

	pool, err := goetty.NewIOSessionPool(1, 2, func() (goetty.IOSession, error) {
		return t.createConn(id)
	})
	if err != nil {
		return nil, err
	}

	if old, loaded := t.conns.LoadOrStore(id, pool); loaded {
		return old.(goetty.IOSessionPool).Get()
	}
	return pool.(goetty.IOSessionPool).Get()
}

func (t *transport) checkConnect(id uint64, conn goetty.IOSession) bool {
	if nil == conn {
		return false
	}

	if conn.IsConnected() {
		return true
	}

	ok, err := conn.Connect()
	if err != nil {
		logger.Errorf("connect to store %d failed with %+v",
			id,
			err)
		return false
	}

	logger.Infof("connected to store %d", id)
	return ok
}

func (t *transport) createConn(id uint64) (goetty.IOSession, error) {
	addr, err := t.resolverFunc(id)
	if err != nil {
		return nil, err
	}

	return goetty.NewConnector(addr,
		goetty.WithClientDecoder(decoder),
		goetty.WithClientEncoder(encoder)), nil
}

func (t *transport) resolverStoreAddr(storeID uint64) (string, error) {
	addr, ok := t.addrs.Load(storeID)

	if !ok {
		addr, ok = t.addrs.Load(storeID)
		if ok {
			return addr.(string), nil
		}

		container, err := t.store.pd.GetStore().GetContainer(storeID)
		if err != nil {
			return "", err
		}

		if container == nil {
			return "", fmt.Errorf("store %d not registered", storeID)
		}

		addr = container.(*containerAdapter).meta.ShardAddr
		t.addrs.Store(storeID, addr)
		t.addrsRevert.Store(addr, storeID)
	}

	return addr.(string), nil
}
