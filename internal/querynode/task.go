// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package querynode

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"runtime/debug"
	"time"

	"go.uber.org/zap"

	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/proto/commonpb"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	queryPb "github.com/milvus-io/milvus/internal/proto/querypb"
	"github.com/milvus-io/milvus/internal/util/funcutil"
)

type task interface {
	ID() UniqueID       // return ReqID
	SetID(uid UniqueID) // set ReqID
	Timestamp() Timestamp
	PreExecute(ctx context.Context) error
	Execute(ctx context.Context) error
	PostExecute(ctx context.Context) error
	WaitToFinish() error
	Notify(err error)
	OnEnqueue() error
}

type baseTask struct {
	done chan error
	ctx  context.Context
	id   UniqueID
}

type addQueryChannelTask struct {
	baseTask
	req  *queryPb.AddQueryChannelRequest
	node *QueryNode
}

type watchDmChannelsTask struct {
	baseTask
	req  *queryPb.WatchDmChannelsRequest
	node *QueryNode
}

type watchDeltaChannelsTask struct {
	baseTask
	req  *queryPb.WatchDeltaChannelsRequest
	node *QueryNode
}

type loadSegmentsTask struct {
	baseTask
	req  *queryPb.LoadSegmentsRequest
	node *QueryNode
}

type releaseCollectionTask struct {
	baseTask
	req  *queryPb.ReleaseCollectionRequest
	node *QueryNode
}

type releasePartitionsTask struct {
	baseTask
	req  *queryPb.ReleasePartitionsRequest
	node *QueryNode
}

func (b *baseTask) ID() UniqueID {
	return b.id
}

func (b *baseTask) SetID(uid UniqueID) {
	b.id = uid
}

func (b *baseTask) WaitToFinish() error {
	err := <-b.done
	return err
}

func (b *baseTask) Notify(err error) {
	b.done <- err
}

// addQueryChannel
func (r *addQueryChannelTask) Timestamp() Timestamp {
	if r.req.Base == nil {
		log.Warn("nil base req in addQueryChannelTask", zap.Any("collectionID", r.req.CollectionID))
		return 0
	}
	return r.req.Base.Timestamp
}

func (r *addQueryChannelTask) OnEnqueue() error {
	if r.req == nil || r.req.Base == nil {
		r.SetID(rand.Int63n(100000000000))
	} else {
		r.SetID(r.req.Base.MsgID)
	}
	return nil
}

func (r *addQueryChannelTask) PreExecute(ctx context.Context) error {
	return nil
}

func (r *addQueryChannelTask) Execute(ctx context.Context) error {
	log.Info("Execute addQueryChannelTask",
		zap.Any("collectionID", r.req.CollectionID))

	collectionID := r.req.CollectionID
	if r.node.queryShardService == nil {
		return fmt.Errorf("null query shard service, collectionID %d", collectionID)
	}

	qc := r.node.queryShardService.getQueryChannel(collectionID)
	log.Info("add query channel for collection", zap.Int64("collectionID", collectionID))

	consumeSubName := funcutil.GenChannelSubName(Params.CommonCfg.QueryNodeSubName, collectionID, Params.QueryNodeCfg.GetNodeID())

	err := qc.AsConsumer(r.req.QueryChannel, consumeSubName, r.req.SeekPosition)
	if err != nil {
		log.Warn("query channel as consumer failed", zap.Int64("collectionID", collectionID), zap.String("channel", r.req.QueryChannel), zap.Error(err))
		return err
	}

	// init global sealed segments
	/*
		for _, segment := range r.req.GlobalSealedSegments {
			sc.globalSegmentManager.addGlobalSegmentInfo(segment)
		}*/

	qc.Start()
	log.Info("addQueryChannelTask done",
		zap.Any("collectionID", r.req.CollectionID),
	)
	return nil
}

func (r *addQueryChannelTask) PostExecute(ctx context.Context) error {
	return nil
}

// watchDmChannelsTask
func (w *watchDmChannelsTask) Timestamp() Timestamp {
	if w.req.Base == nil {
		log.Warn("nil base req in watchDmChannelsTask", zap.Any("collectionID", w.req.CollectionID))
		return 0
	}
	return w.req.Base.Timestamp
}

func (w *watchDmChannelsTask) OnEnqueue() error {
	if w.req == nil || w.req.Base == nil {
		w.SetID(rand.Int63n(100000000000))
	} else {
		w.SetID(w.req.Base.MsgID)
	}
	return nil
}

func (w *watchDmChannelsTask) PreExecute(ctx context.Context) error {
	return nil
}

func (w *watchDmChannelsTask) Execute(ctx context.Context) error {
	collectionID := w.req.CollectionID
	partitionIDs := w.req.GetPartitionIDs()

	lType := w.req.GetLoadMeta().GetLoadType()
	if lType == queryPb.LoadType_UnKnownType {
		// if no partitionID is specified, load type is load collection
		if len(partitionIDs) != 0 {
			lType = queryPb.LoadType_LoadPartition
		} else {
			lType = queryPb.LoadType_LoadCollection
		}
	}

	// get all vChannels
	vChannels := make([]Channel, 0)
	pChannels := make([]Channel, 0)
	VPChannels := make(map[string]string) // map[vChannel]pChannel
	for _, info := range w.req.Infos {
		v := info.ChannelName
		p := funcutil.ToPhysicalChannel(info.ChannelName)
		vChannels = append(vChannels, v)
		pChannels = append(pChannels, p)
		VPChannels[v] = p
	}

	if len(VPChannels) != len(vChannels) {
		return errors.New("get physical channels failed, illegal channel length, collectionID = " + fmt.Sprintln(collectionID))
	}

	log.Info("Starting WatchDmChannels ...",
		zap.String("collectionName", w.req.Schema.Name),
		zap.Int64("collectionID", collectionID),
		zap.Int64("replicaID", w.req.GetReplicaID()),
		zap.Any("load type", lType),
		zap.Strings("vChannels", vChannels),
		zap.Strings("pChannels", pChannels),
	)

	// init collection meta
	sCol := w.node.streaming.replica.addCollection(collectionID, w.req.Schema)
	hCol := w.node.historical.replica.addCollection(collectionID, w.req.Schema)

	//add shard cluster
	for _, vchannel := range vChannels {
		w.node.ShardClusterService.addShardCluster(w.req.GetCollectionID(), w.req.GetReplicaID(), vchannel)
	}

	// load growing segments
	unFlushedSegments := make([]*queryPb.SegmentLoadInfo, 0)
	unFlushedSegmentIDs := make([]UniqueID, 0)
	for _, info := range w.req.Infos {
		for _, ufInfo := range info.UnflushedSegments {
			// unFlushed segment may not have binLogs, skip loading
			if len(ufInfo.Binlogs) > 0 {
				unFlushedSegments = append(unFlushedSegments, &queryPb.SegmentLoadInfo{
					SegmentID:    ufInfo.ID,
					PartitionID:  ufInfo.PartitionID,
					CollectionID: ufInfo.CollectionID,
					BinlogPaths:  ufInfo.Binlogs,
					NumOfRows:    ufInfo.NumOfRows,
					Statslogs:    ufInfo.Statslogs,
					Deltalogs:    ufInfo.Deltalogs,
				})
				unFlushedSegmentIDs = append(unFlushedSegmentIDs, ufInfo.ID)
			}
		}
	}
	req := &queryPb.LoadSegmentsRequest{
		Base: &commonpb.MsgBase{
			MsgType: commonpb.MsgType_LoadSegments,
			MsgID:   w.req.Base.MsgID, // use parent task's msgID
		},
		Infos:        unFlushedSegments,
		CollectionID: collectionID,
		Schema:       w.req.GetSchema(),
		LoadMeta:     w.req.GetLoadMeta(),
	}

	// update partition info from unFlushedSegments and loadMeta
	for _, info := range req.Infos {
		w.node.streaming.replica.addPartition(collectionID, info.PartitionID)
		w.node.historical.replica.addPartition(collectionID, info.PartitionID)
	}
	for _, partitionID := range req.GetLoadMeta().GetPartitionIDs() {
		w.node.historical.replica.addPartition(collectionID, partitionID)
		w.node.streaming.replica.addPartition(collectionID, partitionID)
	}

	log.Info("loading growing segments in WatchDmChannels...",
		zap.Int64("collectionID", collectionID),
		zap.Int64s("unFlushedSegmentIDs", unFlushedSegmentIDs),
	)
	err := w.node.loader.loadSegment(req, segmentTypeGrowing)
	if err != nil {
		log.Warn(err.Error())
		return err
	}
	log.Info("successfully load growing segments done in WatchDmChannels",
		zap.Int64("collectionID", collectionID),
		zap.Int64s("unFlushedSegmentIDs", unFlushedSegmentIDs),
	)

	// remove growing segment if watch dmChannels failed
	defer func() {
		if err != nil {
			for _, segmentID := range unFlushedSegmentIDs {
				w.node.streaming.replica.removeSegment(segmentID)
			}
		}
	}()

	consumeSubName := funcutil.GenChannelSubName(Params.CommonCfg.QueryNodeSubName, collectionID, Params.QueryNodeCfg.GetNodeID())

	// group channels by to seeking or consuming
	channel2SeekPosition := make(map[string]*internalpb.MsgPosition)
	channel2AsConsumerPosition := make(map[string]*internalpb.MsgPosition)
	for _, info := range w.req.Infos {
		if info.SeekPosition == nil || len(info.SeekPosition.MsgID) == 0 {
			channel2AsConsumerPosition[info.ChannelName] = info.SeekPosition
			continue
		}
		info.SeekPosition.MsgGroup = consumeSubName
		channel2SeekPosition[info.ChannelName] = info.SeekPosition
	}
	log.Info("watchDMChannel, group channels done", zap.Int64("collectionID", collectionID))

	// add excluded segments for unFlushed segments,
	// unFlushed segments before check point should be filtered out.
	unFlushedCheckPointInfos := make([]*datapb.SegmentInfo, 0)
	for _, info := range w.req.Infos {
		unFlushedCheckPointInfos = append(unFlushedCheckPointInfos, info.UnflushedSegments...)
	}
	w.node.streaming.replica.addExcludedSegments(collectionID, unFlushedCheckPointInfos)
	unflushedSegmentIDs := make([]UniqueID, 0)
	for i := 0; i < len(unFlushedCheckPointInfos); i++ {
		unflushedSegmentIDs = append(unflushedSegmentIDs, unFlushedCheckPointInfos[i].GetID())
	}
	log.Info("watchDMChannel, add check points info for unFlushed segments done",
		zap.Int64("collectionID", collectionID),
		zap.Any("unflushedSegmentIDs", unflushedSegmentIDs),
	)

	// add excluded segments for flushed segments,
	// flushed segments with later check point than seekPosition should be filtered out.
	flushedCheckPointInfos := make([]*datapb.SegmentInfo, 0)
	for _, info := range w.req.Infos {
		for _, flushedSegment := range info.FlushedSegments {
			for _, position := range channel2SeekPosition {
				if flushedSegment.DmlPosition != nil &&
					flushedSegment.DmlPosition.ChannelName == position.ChannelName &&
					flushedSegment.DmlPosition.Timestamp > position.Timestamp {
					flushedCheckPointInfos = append(flushedCheckPointInfos, flushedSegment)
				}
			}
		}
	}
	w.node.streaming.replica.addExcludedSegments(collectionID, flushedCheckPointInfos)
	log.Info("watchDMChannel, add check points info for flushed segments done",
		zap.Int64("collectionID", collectionID),
		zap.Any("flushedCheckPointInfos", flushedCheckPointInfos),
	)

	// add excluded segments for dropped segments,
	// dropped segments with later check point than seekPosition should be filtered out.
	droppedCheckPointInfos := make([]*datapb.SegmentInfo, 0)
	for _, info := range w.req.Infos {
		for _, droppedSegment := range info.DroppedSegments {
			for _, position := range channel2SeekPosition {
				if droppedSegment != nil &&
					droppedSegment.DmlPosition.ChannelName == position.ChannelName &&
					droppedSegment.DmlPosition.Timestamp > position.Timestamp {
					droppedCheckPointInfos = append(droppedCheckPointInfos, droppedSegment)
				}
			}
		}
	}
	w.node.streaming.replica.addExcludedSegments(collectionID, droppedCheckPointInfos)
	log.Info("watchDMChannel, add check points info for dropped segments done",
		zap.Int64("collectionID", collectionID),
		zap.Any("droppedCheckPointInfos", droppedCheckPointInfos),
	)

	// add flow graph
	channel2FlowGraph, err := w.node.dataSyncService.addFlowGraphsForDMLChannels(collectionID, vChannels)
	if err != nil {
		log.Warn("watchDMChannel, add flowGraph for dmChannels failed", zap.Int64("collectionID", collectionID), zap.Strings("vChannels", vChannels), zap.Error(err))
		return err
	}
	log.Info("Query node add DML flow graphs", zap.Int64("collectionID", collectionID), zap.Any("channels", vChannels))

	// channels as consumer
	for channel, fg := range channel2FlowGraph {
		if _, ok := channel2AsConsumerPosition[channel]; ok {
			// use pChannel to consume
			err = fg.consumeFlowGraph(VPChannels[channel], consumeSubName)
			if err != nil {
				log.Error("msgStream as consumer failed for dmChannels", zap.Int64("collectionID", collectionID), zap.String("vChannel", channel))
				break
			}
		}

		if pos, ok := channel2SeekPosition[channel]; ok {
			pos.MsgGroup = consumeSubName
			// use pChannel to seek
			pos.ChannelName = VPChannels[channel]
			err = fg.seekQueryNodeFlowGraph(pos)
			if err != nil {
				log.Error("msgStream seek failed for dmChannels", zap.Int64("collectionID", collectionID), zap.String("vChannel", channel))
				break
			}
		}
	}

	if err != nil {
		log.Warn("watchDMChannel, add flowGraph for dmChannels failed", zap.Int64("collectionID", collectionID), zap.Strings("vChannels", vChannels), zap.Error(err))
		for _, fg := range channel2FlowGraph {
			fg.flowGraph.Close()
		}
		gcChannels := make([]Channel, 0)
		for channel := range channel2FlowGraph {
			gcChannels = append(gcChannels, channel)
		}
		w.node.dataSyncService.removeFlowGraphsByDMLChannels(gcChannels)
		return err
	}

	log.Info("watchDMChannel, add flowGraph for dmChannels success", zap.Int64("collectionID", collectionID), zap.Strings("vChannels", vChannels))

	sCol.addVChannels(vChannels)
	sCol.addPChannels(pChannels)
	sCol.setLoadType(lType)

	hCol.addVChannels(vChannels)
	hCol.addPChannels(pChannels)
	hCol.setLoadType(lType)
	log.Info("watchDMChannel, init replica done", zap.Int64("collectionID", collectionID), zap.Strings("vChannels", vChannels))

	// create tSafe
	for _, channel := range vChannels {
		w.node.tSafeReplica.addTSafe(channel)
	}

	// add tsafe watch in query shard if exists
	for _, dmlChannel := range vChannels {
		if !w.node.queryShardService.hasQueryShard(dmlChannel) {
			w.node.queryShardService.addQueryShard(collectionID, dmlChannel, w.req.GetReplicaID())
		}

		qs, err := w.node.queryShardService.getQueryShard(dmlChannel)
		if err != nil {
			log.Warn("failed to get query shard", zap.String("dmlChannel", dmlChannel), zap.Error(err))
			continue
		}
		err = qs.watchDMLTSafe()
		if err != nil {
			log.Warn("failed to start query shard watch dml tsafe", zap.Error(err))
		}
	}

	// start flow graphs
	for _, fg := range channel2FlowGraph {
		fg.flowGraph.Start()
	}

	log.Info("WatchDmChannels done", zap.Int64("collectionID", collectionID), zap.Strings("vChannels", vChannels))
	return nil
}

func (w *watchDmChannelsTask) PostExecute(ctx context.Context) error {
	return nil
}

// watchDeltaChannelsTask
func (w *watchDeltaChannelsTask) Timestamp() Timestamp {
	if w.req.Base == nil {
		log.Warn("nil base req in watchDeltaChannelsTask", zap.Any("collectionID", w.req.CollectionID))
		return 0
	}
	return w.req.Base.Timestamp
}

func (w *watchDeltaChannelsTask) OnEnqueue() error {
	if w.req == nil || w.req.Base == nil {
		w.SetID(rand.Int63n(100000000000))
	} else {
		w.SetID(w.req.Base.MsgID)
	}
	return nil
}

func (w *watchDeltaChannelsTask) PreExecute(ctx context.Context) error {
	return nil
}

func (w *watchDeltaChannelsTask) Execute(ctx context.Context) error {
	collectionID := w.req.CollectionID

	// get all vChannels
	vDeltaChannels := make([]Channel, 0)
	pDeltaChannels := make([]Channel, 0)
	VPDeltaChannels := make(map[string]string) // map[vChannel]pChannel
	vChannel2SeekPosition := make(map[string]*internalpb.MsgPosition)
	for _, info := range w.req.Infos {
		v := info.ChannelName
		p := funcutil.ToPhysicalChannel(info.ChannelName)
		vDeltaChannels = append(vDeltaChannels, v)
		pDeltaChannels = append(pDeltaChannels, p)
		VPDeltaChannels[v] = p
		vChannel2SeekPosition[v] = info.SeekPosition
	}
	log.Info("Starting WatchDeltaChannels ...",
		zap.Any("collectionID", collectionID),
		zap.Any("vDeltaChannels", vDeltaChannels),
		zap.Any("pChannels", pDeltaChannels),
	)
	if len(VPDeltaChannels) != len(vDeltaChannels) {
		return errors.New("get physical channels failed, illegal channel length, collectionID = " + fmt.Sprintln(collectionID))
	}
	log.Info("Get physical channels done",
		zap.Any("collectionID", collectionID),
	)

	if hasCollectionInHistorical := w.node.historical.replica.hasCollection(collectionID); !hasCollectionInHistorical {
		return fmt.Errorf("cannot find collection with collectionID, %d", collectionID)
	}
	hCol, err := w.node.historical.replica.getCollectionByID(collectionID)
	if err != nil {
		return err
	}

	if hasCollectionInStreaming := w.node.streaming.replica.hasCollection(collectionID); !hasCollectionInStreaming {
		return fmt.Errorf("cannot find collection with collectionID, %d", collectionID)
	}
	sCol, err := w.node.streaming.replica.getCollectionByID(collectionID)
	if err != nil {
		return err
	}

	channel2FlowGraph, err := w.node.dataSyncService.addFlowGraphsForDeltaChannels(collectionID, vDeltaChannels)
	if err != nil {
		log.Warn("watchDeltaChannel, add flowGraph for deltaChannel failed", zap.Int64("collectionID", collectionID), zap.Strings("vDeltaChannels", vDeltaChannels), zap.Error(err))
		return err
	}
	consumeSubName := funcutil.GenChannelSubName(Params.CommonCfg.QueryNodeSubName, collectionID, Params.QueryNodeCfg.GetNodeID())
	// channels as consumer
	for channel, fg := range channel2FlowGraph {
		// use pChannel to consume
		err = fg.consumeFlowGraphFromLatest(VPDeltaChannels[channel], consumeSubName)
		if err != nil {
			log.Error("msgStream as consumer failed for deltaChannels", zap.Int64("collectionID", collectionID), zap.Strings("vDeltaChannels", vDeltaChannels))
			break
		}
		err = w.node.loader.FromDmlCPLoadDelete(w.ctx, collectionID, vChannel2SeekPosition[channel])
		if err != nil {
			log.Error("watchDeltaChannelsTask from dml cp load delete failed", zap.Int64("collectionID", collectionID), zap.Strings("vDeltaChannels", vDeltaChannels))
			break
		}
	}
	if err != nil {
		log.Warn("watchDeltaChannel, add flowGraph for deltaChannel failed", zap.Int64("collectionID", collectionID), zap.Strings("vDeltaChannels", vDeltaChannels), zap.Error(err))
		for _, fg := range channel2FlowGraph {
			fg.flowGraph.Close()
		}
		gcChannels := make([]Channel, 0)
		for channel := range channel2FlowGraph {
			gcChannels = append(gcChannels, channel)
		}
		w.node.dataSyncService.removeFlowGraphsByDeltaChannels(gcChannels)
		return err
	}

	log.Info("watchDeltaChannel, add flowGraph for deltaChannel success", zap.Int64("collectionID", collectionID), zap.Strings("vDeltaChannels", vDeltaChannels))

	//set collection replica
	hCol.addVDeltaChannels(vDeltaChannels)
	hCol.addPDeltaChannels(pDeltaChannels)

	sCol.addVDeltaChannels(vDeltaChannels)
	sCol.addPDeltaChannels(pDeltaChannels)

	// create tSafe
	for _, channel := range vDeltaChannels {
		w.node.tSafeReplica.addTSafe(channel)
	}

	// add tsafe watch in query shard if exists
	for _, channel := range vDeltaChannels {
		dmlChannel, err := funcutil.ConvertChannelName(channel, Params.CommonCfg.RootCoordDelta, Params.CommonCfg.RootCoordDml)
		if err != nil {
			log.Warn("failed to convert delta channel to dml", zap.String("channel", channel), zap.Error(err))
			continue
		}
		if !w.node.queryShardService.hasQueryShard(dmlChannel) {
			w.node.queryShardService.addQueryShard(collectionID, dmlChannel, w.req.GetReplicaId())
		}

		qs, err := w.node.queryShardService.getQueryShard(dmlChannel)
		if err != nil {
			log.Warn("failed to get query shard", zap.String("dmlChannel", dmlChannel), zap.Error(err))
			continue
		}
		err = qs.watchDeltaTSafe()
		if err != nil {
			log.Warn("failed to start query shard watch delta tsafe", zap.Error(err))
		}
	}

	// start flow graphs
	for _, fg := range channel2FlowGraph {
		fg.flowGraph.Start()
	}

	log.Info("WatchDeltaChannels done", zap.Int64("collectionID", collectionID), zap.String("ChannelIDs", fmt.Sprintln(vDeltaChannels)))
	return nil
}

func (w *watchDeltaChannelsTask) PostExecute(ctx context.Context) error {
	return nil
}

// loadSegmentsTask
func (l *loadSegmentsTask) Timestamp() Timestamp {
	if l.req.Base == nil {
		log.Warn("nil base req in loadSegmentsTask")
		return 0
	}
	return l.req.Base.Timestamp
}

func (l *loadSegmentsTask) OnEnqueue() error {
	if l.req == nil || l.req.Base == nil {
		l.SetID(rand.Int63n(100000000000))
	} else {
		l.SetID(l.req.Base.MsgID)
	}
	return nil
}

func (l *loadSegmentsTask) PreExecute(ctx context.Context) error {
	return nil
}

func (l *loadSegmentsTask) Execute(ctx context.Context) error {
	// TODO: support db
	log.Info("LoadSegment start", zap.Int64("msgID", l.req.Base.MsgID))
	var err error

	// init meta
	collectionID := l.req.GetCollectionID()
	l.node.historical.replica.addCollection(collectionID, l.req.GetSchema())
	l.node.streaming.replica.addCollection(collectionID, l.req.GetSchema())
	for _, partitionID := range l.req.GetLoadMeta().GetPartitionIDs() {
		err = l.node.historical.replica.addPartition(collectionID, partitionID)
		if err != nil {
			return err
		}
		err = l.node.streaming.replica.addPartition(collectionID, partitionID)
		if err != nil {
			return err
		}
	}

	err = l.node.loader.loadSegment(l.req, segmentTypeSealed)
	if err != nil {
		log.Warn(err.Error())
		return err
	}

	log.Info("LoadSegments done", zap.Int64("msgID", l.req.Base.MsgID))
	return nil
}

func (l *loadSegmentsTask) PostExecute(ctx context.Context) error {
	return nil
}

// releaseCollectionTask
func (r *releaseCollectionTask) Timestamp() Timestamp {
	if r.req.Base == nil {
		log.Warn("nil base req in releaseCollectionTask", zap.Any("collectionID", r.req.CollectionID))
		return 0
	}
	return r.req.Base.Timestamp
}

func (r *releaseCollectionTask) OnEnqueue() error {
	if r.req == nil || r.req.Base == nil {
		r.SetID(rand.Int63n(100000000000))
	} else {
		r.SetID(r.req.Base.MsgID)
	}
	return nil
}

func (r *releaseCollectionTask) PreExecute(ctx context.Context) error {
	return nil
}

type ReplicaType int

const (
	replicaNone ReplicaType = iota
	replicaStreaming
	replicaHistorical
)

func (r *releaseCollectionTask) Execute(ctx context.Context) error {
	log.Info("Execute release collection task", zap.Any("collectionID", r.req.CollectionID))
	// sleep to wait for query tasks done
	const gracefulReleaseTime = 1
	time.Sleep(gracefulReleaseTime * time.Second)
	log.Info("Starting release collection...",
		zap.Any("collectionID", r.req.CollectionID),
	)

	err := r.releaseReplica(r.node.streaming.replica, replicaStreaming)
	if err != nil {
		return fmt.Errorf("release collection failed, collectionID = %d, err = %s", r.req.CollectionID, err)
	}

	// remove collection metas in streaming and historical
	log.Info("release historical", zap.Any("collectionID", r.req.CollectionID))
	err = r.releaseReplica(r.node.historical.replica, replicaHistorical)
	if err != nil {
		return fmt.Errorf("release collection failed, collectionID = %d, err = %s", r.req.CollectionID, err)
	}

	debug.FreeOSMemory()

	r.node.queryShardService.releaseCollection(r.req.CollectionID)

	log.Info("ReleaseCollection done", zap.Int64("collectionID", r.req.CollectionID))
	return nil
}

func (r *releaseCollectionTask) releaseReplica(replica ReplicaInterface, replicaType ReplicaType) error {
	// block search/query operation
	replica.queryLock()

	collection, err := replica.getCollectionByID(r.req.CollectionID)
	if err != nil {
		replica.queryUnlock()
		return err
	}
	// set release time
	log.Info("set release time", zap.Any("collectionID", r.req.CollectionID))
	collection.setReleaseTime(r.req.Base.Timestamp)
	replica.queryUnlock()

	// remove all flow graphs of the target collection
	var channels []Channel
	if replicaType == replicaStreaming {
		channels = collection.getVChannels()
		r.node.dataSyncService.removeFlowGraphsByDMLChannels(channels)
	} else {
		// remove all tSafes and flow graphs of the target collection
		channels = collection.getVDeltaChannels()
		r.node.dataSyncService.removeFlowGraphsByDeltaChannels(channels)
	}

	// remove all tSafes of the target collection
	for _, channel := range channels {
		log.Info("Releasing tSafe in releaseCollectionTask...",
			zap.Any("collectionID", r.req.CollectionID),
			zap.Any("vDeltaChannel", channel),
		)
		r.node.tSafeReplica.removeTSafe(channel)
	}

	// remove excludedSegments record
	replica.removeExcludedSegments(r.req.CollectionID)
	err = replica.removeCollection(r.req.CollectionID)
	if err != nil {
		return err
	}
	return nil
}

func (r *releaseCollectionTask) PostExecute(ctx context.Context) error {
	return nil
}

// releasePartitionsTask
func (r *releasePartitionsTask) Timestamp() Timestamp {
	if r.req.Base == nil {
		log.Warn("nil base req in releasePartitionsTask", zap.Any("collectionID", r.req.CollectionID))
		return 0
	}
	return r.req.Base.Timestamp
}

func (r *releasePartitionsTask) OnEnqueue() error {
	if r.req == nil || r.req.Base == nil {
		r.SetID(rand.Int63n(100000000000))
	} else {
		r.SetID(r.req.Base.MsgID)
	}
	return nil
}

func (r *releasePartitionsTask) PreExecute(ctx context.Context) error {
	return nil
}

func (r *releasePartitionsTask) Execute(ctx context.Context) error {
	log.Info("Execute release partition task",
		zap.Any("collectionID", r.req.CollectionID),
		zap.Any("partitionIDs", r.req.PartitionIDs))

	// sleep to wait for query tasks done
	const gracefulReleaseTime = 1
	time.Sleep(gracefulReleaseTime * time.Second)

	// get collection from streaming and historical
	_, err := r.node.historical.replica.getCollectionByID(r.req.CollectionID)
	if err != nil {
		return fmt.Errorf("release partitions failed, collectionID = %d, err = %s", r.req.CollectionID, err)
	}
	_, err = r.node.streaming.replica.getCollectionByID(r.req.CollectionID)
	if err != nil {
		return fmt.Errorf("release partitions failed, collectionID = %d, err = %s", r.req.CollectionID, err)
	}
	log.Info("start release partition", zap.Any("collectionID", r.req.CollectionID))

	for _, id := range r.req.PartitionIDs {
		// remove partition from streaming and historical
		hasPartitionInHistorical := r.node.historical.replica.hasPartition(id)
		if hasPartitionInHistorical {
			err := r.node.historical.replica.removePartition(id)
			if err != nil {
				// not return, try to release all partitions
				log.Warn(err.Error())
			}
		}
		hasPartitionInStreaming := r.node.streaming.replica.hasPartition(id)
		if hasPartitionInStreaming {
			err := r.node.streaming.replica.removePartition(id)
			if err != nil {
				// not return, try to release all partitions
				log.Warn(err.Error())
			}
		}
	}

	log.Info("Release partition task done",
		zap.Any("collectionID", r.req.CollectionID),
		zap.Any("partitionIDs", r.req.PartitionIDs))
	return nil
}

func (r *releasePartitionsTask) PostExecute(ctx context.Context) error {
	return nil
}
