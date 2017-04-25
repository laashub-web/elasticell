// Copyright 2016 DeepFabric, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package raftstore

import (
	"bytes"

	"time"

	"github.com/coreos/etcd/raft/raftpb"
	"github.com/deepfabric/elasticell/pkg/log"
	"github.com/deepfabric/elasticell/pkg/pb/metapb"
	"github.com/deepfabric/elasticell/pkg/pb/mraft"
	"github.com/deepfabric/elasticell/pkg/pb/pdpb"
	"github.com/deepfabric/elasticell/pkg/pb/raftcmdpb"
	"github.com/deepfabric/elasticell/pkg/pd"
	"github.com/deepfabric/elasticell/pkg/storage"
	"github.com/deepfabric/elasticell/pkg/util"
	"github.com/deepfabric/elasticell/pkg/util/uuid"
	"golang.org/x/net/context"
)

var (
	defaultJobQueueCap uint64 = 10000
)

const (
	applyWorker            = "apply-work"
	snapWorker             = "snap-work"
	pdWorker               = "pd-work"
	splitWorker            = "split-work"
	defaultWorkerQueueSize = 64
	defaultNotifyChanSize  = 1024
)

// Store is the store for raft
type Store struct {
	cfg *Cfg

	id        uint64
	clusterID uint64
	meta      metapb.Store

	pdClient *pd.Client

	replicatesMap *cellPeersMap // cellid -> peer replicate
	keyRanges     *util.CellTree
	peerCache     *peerCacheMap
	delegates     *applyDelegateMap
	pendingCells  []metapb.Cell

	trans      *transport
	engine     storage.Driver
	runner     *util.Runner
	notifyChan chan interface{}
}

// NewStore returns store
func NewStore(clusterID uint64, pdClient *pd.Client, meta metapb.Store, engine storage.Driver, cfg *Cfg) *Store {
	s := new(Store)
	s.clusterID = clusterID
	s.id = meta.ID
	s.meta = meta
	s.engine = engine
	s.cfg = cfg
	s.pdClient = pdClient

	s.trans = newTransport(s.cfg.Raft, s.notify)
	s.notifyChan = make(chan interface{}, defaultNotifyChanSize)

	s.keyRanges = util.NewCellTree()
	s.replicatesMap = newCellPeersMap()
	s.peerCache = newPeerCacheMap()
	s.delegates = newApplyDelegateMap()

	s.runner = util.NewRunner()
	s.runner.AddNamedWorker(snapWorker, defaultWorkerQueueSize)
	s.runner.AddNamedWorker(applyWorker, defaultWorkerQueueSize)
	s.runner.AddNamedWorker(pdWorker, defaultWorkerQueueSize)
	s.runner.AddNamedWorker(splitWorker, defaultWorkerQueueSize)

	s.init()

	return s
}

func (s *Store) init() {
	totalCount := 0
	tomebstoneCount := 0
	applyingCount := 0

	err := s.getMetaEngine().Scan(cellMetaMinKey, cellMetaMaxKey, func(key, value []byte) (bool, error) {
		cellID, suffix, err := decodeCellMetaKey(key)
		if err != nil {
			return false, err
		}

		if suffix != cellStateSuffix {
			return true, nil
		}

		totalCount++

		localState := new(mraft.CellLocalState)
		util.MustUnmarshal(localState, value)

		if localState.State == mraft.Tombstone {
			tomebstoneCount++
			log.Infof("bootstrap: cell is tombstone in store, cellID=<%d>", cellID)
			return true, nil
		}

		pr, err := createPeerReplicate(s, &localState.Cell)
		if err != nil {
			return false, err
		}

		if localState.State == mraft.Applying {
			applyingCount++
			log.Infof("bootstrap: cell is applying in store, cellID=<%d>", cellID)
			pr.startApplyingSnapJob()
		}

		s.keyRanges.Update(localState.Cell)
		s.replicatesMap.put(cellID, pr)

		return true, nil
	})

	if err != nil {
		log.Fatalf("bootstrap: init store failed, errors:\n %+v", err)
	}

	log.Infof("bootstrap: starts with %d cells, including %d tombstones and %d applying	cells",
		totalCount,
		tomebstoneCount,
		applyingCount)

	s.cleanup()
}

func (s *Store) cleanup() {
	// clean up all possible garbage data
	lastStartKey := getDataKey([]byte(""))

	s.keyRanges.Ascend(func(cell *metapb.Cell) bool {
		start := encStartKey(cell)
		err := s.getDataEngine().RangeDelete(lastStartKey, start)
		if err != nil {
			log.Fatalf("bootstrap: cleanup possible garbage data failed, start=<%v> end=<%v> errors:\n %+v",
				lastStartKey,
				start,
				err)
		}

		lastStartKey = encEndKey(cell)
		return true
	})

	err := s.getDataEngine().RangeDelete(lastStartKey, dataMaxKey)
	if err != nil {
		log.Fatalf("bootstrap: cleanup possible garbage data failed, start=<%v> end=<%v> errors:\n %+v",
			lastStartKey,
			dataMaxKey,
			err)
	}

	log.Infof("bootstrap: cleanup possible garbage data complete")
}

// Start returns the error when start store
func (s *Store) Start() {
	// TODO: impl
	go s.startTransfer()
	<-s.trans.server.Started()

	s.startHandleNotifyMsg()
	s.startStoreHeartbeatTask()
	s.startCellHeartbeatTask()
	s.startGCTask()
	s.startCellSplitCheckTask()
}

func (s *Store) startTransfer() {
	err := s.trans.start()
	if err != nil {
		log.Fatalf("bootstrap: start transfer failed, errors:\n %+v", err)
	}
}

func (s *Store) startHandleNotifyMsg() {
	s.runner.RunCancelableTask(func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				log.Infof("stop: store msg handler stopped")
				return
			case n := <-s.notifyChan:
				if msg, ok := n.(*mraft.RaftMessage); ok {
					s.onRaftMessage(msg)
				} else if msg, ok := n.(*cmd); ok {
					s.onRaftCMD(msg)
				} else if ret, ok := n.(*splitCheckResult); ok {
					s.onSplitCheckResult(ret)
				}
			}
		}
	})
}

func (s *Store) startStoreHeartbeatTask() {
	s.runner.RunCancelableTask(func(ctx context.Context) {
		ticker := time.NewTicker(s.cfg.getStoreHeartbeatDuration())

		var err error
		var job *util.Job

		for {
			select {
			case <-ctx.Done():
				log.Infof("stop: store heart beat job stopped")
				return
			case <-ticker.C:
				if job != nil && job.IsNotComplete() {
					// cancel last if not complete
					job.Cancel()
				} else {
					job, err = s.addPDJob(s.handleStoreHeartbeat)
					if err != nil {
						log.Errorf("heartbeat-store[%d]: add job failed, errors:\n %+v",
							s.GetID(),
							err)
					}
				}
			}
		}
	})
}

func (s *Store) startCellHeartbeatTask() {
	s.runner.RunCancelableTask(func(ctx context.Context) {
		ticker := time.NewTicker(s.cfg.getCellHeartbeatDuration())

		for {
			select {
			case <-ctx.Done():
				log.Infof("stop: cell heart beat job stopped")
				return
			case <-ticker.C:
				s.handleCellHeartbeat()
			}
		}
	})
}

func (s *Store) startGCTask() {
	// TODO: impl

}

func (s *Store) startCellSplitCheckTask() {
	// TODO: impl
	s.runner.RunCancelableTask(func(ctx context.Context) {
		ticker := time.NewTicker(s.cfg.getSplitCellCheckDuration())

		for {
			select {
			case <-ctx.Done():
				log.Infof("stop: cell heart beat job stopped")
				return
			case <-ticker.C:
				s.handleCellSplitCheck()
			}
		}
	})
}

// Stop returns the error when stop store
func (s *Store) Stop() error {
	err := s.runner.Stop()
	s.trans.stop()
	return err
}

// GetID returns store id
func (s *Store) GetID() uint64 {
	return s.id
}

// GetMeta returns store meta
func (s *Store) GetMeta() metapb.Store {
	return s.meta
}

func (s *Store) notify(msg interface{}) {
	s.notifyChan <- msg
}

func (s *Store) onRaftMessage(msg *mraft.RaftMessage) {
	if log.DebugEnabled() {
		log.Debugf("raftstore[store-%d]: receive a raft message, fromPeer=<%d> toPeer=<%d> msg=<%s>",
			s.id,
			msg.FromPeer.ID,
			msg.ToPeer.ID,
			msg.String())
	}

	if !s.isRaftMsgValid(msg) {
		return
	}

	if msg.IsTombstone {
		// we receive a message tells us to remove ourself.
		s.handleGCPeerMsg(msg)
		return
	}

	yes, err := s.isMsgStale(msg)
	if err != nil || yes {
		return
	}

	if !s.tryToCreatePeerReplicate(msg.CellID, msg) {
		return
	}

	ok, err := s.checkSnapshot(msg)
	if err != nil {
		return
	}
	if !ok {
		return
	}

	s.addPeerToCache(msg.FromPeer)

	pr := s.getPeerReplicate(msg.CellID)
	err = pr.step(msg.Message)
	if err != nil {
		return
	}
}

func (s *Store) onRaftCMD(cmd *cmd) {
	// we handle all read, write and admin cmd here
	if cmd.req.Header == nil || cmd.req.Header.UUID == nil {
		cmd.resp(errorOtherCMDResp(errMissingUUIDCMD))
		return
	}

	err := s.validateStoreID(cmd.req)
	if err != nil {
		cmd.respOtherError(err)
		return
	}

	pr := s.replicatesMap.get(cmd.req.Header.CellId)
	term := pr.getCurrentTerm()

	pe := s.validateCell(cmd.req)
	if err != nil {
		cmd.resp(errorPbResp(pe, cmd.req.Header.UUID, term))
		return
	}

	// Note:
	// The peer that is being checked is a leader. It might step down to be a follower later. It
	// doesn't matter whether the peer is a leader or not. If it's not a leader, the proposing
	// command log entry can't be committed.
	meta := &proposalMeta{
		uuid: cmd.req.Header.UUID,
		term: term,
	}

	// TODO: Parallel on every PeerReplicate
	pr.propose(meta, cmd)
}

func (s *Store) onSplitCheckResult(result *splitCheckResult) {
	if len(result.splitKey) == 0 {
		log.Errorf("raftstore-split[cell-%d]: split key must not be empty", result.cellID)
		return
	}

	p := s.replicatesMap.get(result.cellID)
	if p == nil || !p.isLeader() {
		log.Errorf("raftstore-split[cell-%d]: cell not exist or not leader, skip", result.cellID)
		return
	}

	cell := p.getCell()

	if cell.Epoch.CellVer != result.epoch.CellVer {
		log.Infof("raftstore-split[cell-%d]: epoch changed, need re-check later, current=<%+v> split=<%+v>",
			result.cellID,
			cell.Epoch,
			result.epoch)
		return
	}

	err := p.startAskSplitJob(cell, p.peer, result.splitKey)
	if err != nil {
		log.Errorf("raftstore-split[cell-%d]: add ask split job failed, errors:\n %+v",
			result.cellID,
			err)
	}
}

func (s *Store) handleGCPeerMsg(msg *mraft.RaftMessage) {
	// TODO: impl
}

func (s *Store) handleStaleMsg(msg *mraft.RaftMessage, currEpoch metapb.CellEpoch, needGC bool) {
	// TODO: impl
}

// If target peer doesn't exist, create it.
//
// return false to indicate that target peer is in invalid state or
// doesn't exist and can't be created.
func (s *Store) tryToCreatePeerReplicate(cellID uint64, msg *mraft.RaftMessage) bool {
	var (
		hasPeer     = false
		asyncRemove = false
		stalePeer   metapb.Peer
	)

	target := msg.ToPeer

	if p := s.getPeerReplicate(cellID); p != nil {
		hasPeer = true

		// we may encounter a message with larger peer id, which means
		// current peer is stale, then we should remove current peer
		if p.peer.ID < target.ID {
			// cancel snapshotting op
			if p.ps.isApplyingSnapshot() && !p.ps.cancelApplyingSnapJob() {
				log.Infof("raftstore[cell-%d]: stale peer is applying snapshot, will destroy next time, peer=<%d>",
					cellID,
					p.peer.ID)

				return false
			}

			stalePeer = p.peer
			asyncRemove = p.getStore().isInitialized()
		} else if p.peer.ID > target.ID {
			log.Infof("raftstore[cell-%d]: may be from peer is stale, targetID=<%d> currentID=<%d>",
				cellID,
				target.ID,
				p.peer.ID)
			return false
		}
	}

	// If we found stale peer, we will destory it
	if stalePeer.ID > 0 {
		// TODO: destroy async
		if asyncRemove {
			s.destroyPeer(cellID, stalePeer, asyncRemove)
			return false
		}

		s.destroyPeer(cellID, stalePeer, false)
		hasPeer = false
	}

	if hasPeer {
		return true
	}

	// arrive here means target peer not found, we will try to create it
	message := msg.Message
	if message.Type != raftpb.MsgVote &&
		(message.Type != raftpb.MsgHeartbeat || message.Commit != invalidIndex) {
		log.Debugf("raftstore[cell-%d]: target peer doesn't exist, peer=<%s> message=<%s>",
			cellID,
			target,
			message.Type)
		return false
	}

	// check range overlapped
	item := s.keyRanges.Search(msg.Start)
	if item.ID > 0 {
		if bytes.Compare(encStartKey(&item), getDataEndKey(msg.End)) < 0 {
			if log.DebugEnabled() {
				log.Debugf("raftstore[cell-%d]: msg is overlapped with cell, cell=<%s> msg=<%s>",
					cellID,
					item.String(),
					msg.String())
			}
			return false
		}
	}

	// now we can create a replicate
	peerReplicate, err := doReplicate(s, cellID, target.ID)
	if err != nil {
		log.Errorf("raftstore[cell-%d]: replicate peer failure, errors:\n %+v",
			cellID,
			err)
		return false
	}

	// following snapshot may overlap, should insert into keyRanges after
	// snapshot is applied.
	s.keyRanges.Update(peerReplicate.getCell())
	return true
}

func (s *Store) destroyPeer(cellID uint64, target metapb.Peer, async bool) {
	log.Infof("raftstore[cell-%d]: destroying stal peer, peer=<%v> async=<%s>",
		cellID,
		target,
		async)

	// TODO: impl
}

func (s *Store) getPeerReplicate(cellID uint64) *PeerReplicate {
	return s.replicatesMap.get(cellID)
}

func (s *Store) addPeerToCache(peer metapb.Peer) {
	s.peerCache.put(peer.ID, peer)
}

func (s *Store) addPDJob(task func() error) (*util.Job, error) {
	return s.addNamedJob(pdWorker, task)
}

func (s *Store) addSnapJob(task func() error) (*util.Job, error) {
	return s.addNamedJob(snapWorker, task)
}

func (s *Store) addApplyJob(task func() error) (*util.Job, error) {
	return s.addNamedJob(applyWorker, task)
}

func (s *Store) addSplitJob(task func() error) (*util.Job, error) {
	return s.addNamedJob(splitWorker, task)
}

func (s *Store) addNamedJob(worker string, task func() error) (*util.Job, error) {
	return s.runner.RunJobWithNamedWorker(worker, task)
}

func (s *Store) handleStoreHeartbeat() error {
	req := new(pdpb.StoreHeartbeatReq)
	req.Header.ClusterID = s.clusterID
	// TODO: impl
	req.Stats = &pdpb.StoreStats{
		StoreID:            s.GetID(),
		Capacity:           0,
		Available:          0,
		CellCount:          s.replicatesMap.size(),
		SendingSnapCount:   0,
		ReceivingSnapCount: 0,
		StartTime:          0,
		ApplyingSnapCount:  0,
		IsBusy:             false,
		UsedSize:           0,
		BytesWritten:       0,
	}

	_, err := s.pdClient.StoreHeartbeat(context.TODO(), req)
	if err != nil {
		log.Errorf("heartbeat-store[%d]: heartbeat failed, errors:\n +%v",
			s.id,
			err)
	}

	return err
}

func (s *Store) handleCellHeartbeat() {
	var err error

	for _, p := range s.replicatesMap.values() {
		p.checkPeers()
	}

	for _, p := range s.replicatesMap.values() {
		if p.isLeader() {
			if p.lastHBJob != nil && p.lastHBJob.IsNotComplete() {
				// cancel last if not complete
				p.lastHBJob.Cancel()
			} else {
				p.lastHBJob, err = s.addPDJob(p.doHeartbeat)
				if err != nil {
					log.Errorf("heartbeat-cell[%d]: add cell heartbeat job failed, errors:\n %+v",
						p.cellID,
						err)
				}
			}
		}
	}
}

func (s *Store) handleCellSplitCheck() {
	if s.runner.IsNamedWorkerBusy(splitWorker) {
		return
	}

	s.replicatesMap.foreach(func(pr *PeerReplicate) (bool, error) {
		if !pr.isLeader() {
			return true, nil
		}

		if pr.sizeDiffHint < s.cfg.CellCheckSizeDiff {
			return true, nil
		}

		log.Infof("raftstore-split[cell-%d]: cell need to check whether should split, diff=<%d> max=<%d>",
			pr.cellID,
			pr.sizeDiffHint,
			s.cfg.CellCheckSizeDiff)

		err := pr.startSplitCheckJob()
		if err != nil {
			log.Errorf("raftstore-split[cell-%d]: add split check job failed, errors:\n %+v",
				pr.cellID,
				err)
			return false, err
		}

		pr.sizeDiffHint = 0
		return true, nil
	})
}

func (s *Store) sendAdminRequest(cell metapb.Cell, peer metapb.Peer, adminReq *raftcmdpb.AdminRequest) {
	req := new(raftcmdpb.RaftCMDRequest)
	req.Header = &raftcmdpb.RaftRequestHeader{
		CellId:    cell.ID,
		CellEpoch: cell.Epoch,
		Peer:      peer,
		UUID:      uuid.NewV4().Bytes(),
	}
	req.AdminRequest = adminReq
	s.notify(newCMD(req, nil))
}

func (s *Store) getEngine(kind storage.Kind) storage.Engine {
	return s.engine.GetEngine(kind)
}

func (s *Store) getMetaEngine() storage.Engine {
	return s.getEngine(storage.Meta)
}

func (s *Store) getDataEngine() storage.Engine {
	return s.getEngine(storage.Data)
}