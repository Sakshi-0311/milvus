// Copyright (C) 2019-2020 Zilliz. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software distributed under the License
// is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
// or implied. See the License for the specific language governing permissions and limitations under the License.

package dataservice

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/milvus-io/milvus/internal/logutil"

	"github.com/golang/protobuf/proto"
	grpcdatanodeclient "github.com/milvus-io/milvus/internal/distributed/datanode/client"
	etcdkv "github.com/milvus-io/milvus/internal/kv/etcd"
	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/msgstream"
	"github.com/milvus-io/milvus/internal/timesync"
	"github.com/milvus-io/milvus/internal/types"
	"github.com/milvus-io/milvus/internal/util/retry"
	"github.com/milvus-io/milvus/internal/util/sessionutil"
	"github.com/milvus-io/milvus/internal/util/typeutil"
	"go.etcd.io/etcd/clientv3"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus/internal/proto/commonpb"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/internal/proto/milvuspb"
)

const role = "dataservice"

type (
	UniqueID  = typeutil.UniqueID
	Timestamp = typeutil.Timestamp
)
type Server struct {
	ctx              context.Context
	serverLoopCtx    context.Context
	serverLoopCancel context.CancelFunc
	serverLoopWg     sync.WaitGroup
	state            atomic.Value
	kvClient         *etcdkv.EtcdKV
	meta             *meta
	segAllocator     segmentAllocatorInterface
	statsHandler     *statsHandler
	allocator        allocatorInterface
	cluster          *dataNodeCluster
	msgProducer      *timesync.MsgProducer
	masterClient     types.MasterService
	ttMsgStream      msgstream.MsgStream
	k2sMsgStream     msgstream.MsgStream
	ddChannelMu      struct {
		sync.Mutex
		name string
	}
	session              *sessionutil.Session
	segmentInfoStream    msgstream.MsgStream
	flushMsgStream       msgstream.MsgStream
	insertChannels       []string
	msFactory            msgstream.Factory
	ttBarrier            timesync.TimeTickBarrier
	createDataNodeClient func(addr string) types.DataNode
}

func CreateServer(ctx context.Context, factory msgstream.Factory) (*Server, error) {
	rand.Seed(time.Now().UnixNano())
	s := &Server{
		ctx:       ctx,
		cluster:   newDataNodeCluster(),
		msFactory: factory,
	}
	s.insertChannels = s.getInsertChannels()
	s.createDataNodeClient = func(addr string) types.DataNode {
		return grpcdatanodeclient.NewClient(addr)
	}
	s.UpdateStateCode(internalpb.StateCode_Abnormal)
	return s, nil
}

func (s *Server) getInsertChannels() []string {
	channels := make([]string, 0, Params.InsertChannelNum)
	var i int64 = 0
	for ; i < Params.InsertChannelNum; i++ {
		channels = append(channels, Params.InsertChannelPrefixName+strconv.FormatInt(i, 10))
	}
	return channels
}

func (s *Server) SetMasterClient(masterClient types.MasterService) {
	s.masterClient = masterClient
}

func (s *Server) Init() error {
	s.session = sessionutil.NewSession(s.ctx, []string{Params.EtcdAddress})
	s.session.Init(typeutil.DataServiceRole, Params.IP, true)
	return nil
}

func (s *Server) Start() error {
	var err error
	m := map[string]interface{}{
		"PulsarAddress":  Params.PulsarAddress,
		"ReceiveBufSize": 1024,
		"PulsarBufSize":  1024}
	err = s.msFactory.SetParams(m)
	if err != nil {
		return err
	}

	if err := s.initMeta(); err != nil {
		return err
	}

	s.allocator = newAllocator(s.masterClient)

	s.startSegmentAllocator()
	s.statsHandler = newStatsHandler(s.meta)
	if err = s.loadMetaFromMaster(); err != nil {
		return err
	}
	if err = s.initMsgProducer(); err != nil {
		return err
	}
	s.startServerLoop()
	s.UpdateStateCode(internalpb.StateCode_Healthy)
	log.Debug("start success")
	return nil
}

func (s *Server) startSegmentAllocator() {
	stream := s.initSegmentInfoChannel()
	helper := createNewSegmentHelper(stream)
	s.segAllocator = newSegmentAllocator(s.meta, s.allocator, withAllocHelper(helper))
}

func (s *Server) initSegmentInfoChannel() msgstream.MsgStream {
	segmentInfoStream, _ := s.msFactory.NewMsgStream(s.ctx)
	segmentInfoStream.AsProducer([]string{Params.SegmentInfoChannelName})
	log.Debug("dataservice AsProducer: " + Params.SegmentInfoChannelName)
	segmentInfoStream.Start()
	return segmentInfoStream
}

func (s *Server) UpdateStateCode(code internalpb.StateCode) {
	s.state.Store(code)
}

func (s *Server) checkStateIsHealthy() bool {
	return s.state.Load().(internalpb.StateCode) == internalpb.StateCode_Healthy
}

func (s *Server) initMeta() error {
	connectEtcdFn := func() error {
		etcdClient, err := clientv3.New(clientv3.Config{Endpoints: []string{Params.EtcdAddress}})
		if err != nil {
			return err
		}
		s.kvClient = etcdkv.NewEtcdKV(etcdClient, Params.MetaRootPath)
		s.meta, err = newMeta(s.kvClient)
		if err != nil {
			return err
		}
		return nil
	}
	return retry.Retry(100000, time.Millisecond*200, connectEtcdFn)
}

func (s *Server) initMsgProducer() error {
	var err error
	if s.ttMsgStream, err = s.msFactory.NewMsgStream(s.ctx); err != nil {
		return err
	}
	s.ttMsgStream.AsConsumer([]string{Params.TimeTickChannelName}, Params.DataServiceSubscriptionName)
	log.Debug("dataservice AsConsumer: " + Params.TimeTickChannelName + " : " + Params.DataServiceSubscriptionName)
	s.ttMsgStream.Start()
	s.ttBarrier = timesync.NewHardTimeTickBarrier(s.ctx, s.ttMsgStream, s.cluster.GetNodeIDs())
	s.ttBarrier.Start()
	if s.k2sMsgStream, err = s.msFactory.NewMsgStream(s.ctx); err != nil {
		return err
	}
	s.k2sMsgStream.AsProducer(Params.K2SChannelNames)
	log.Debug("dataservice AsProducer: " + strings.Join(Params.K2SChannelNames, ", "))
	s.k2sMsgStream.Start()
	dataNodeTTWatcher := newDataNodeTimeTickWatcher(s.meta, s.segAllocator, s.cluster)
	k2sMsgWatcher := timesync.NewMsgTimeTickWatcher(s.k2sMsgStream)
	if s.msgProducer, err = timesync.NewTimeSyncMsgProducer(s.ttBarrier, dataNodeTTWatcher, k2sMsgWatcher); err != nil {
		return err
	}
	s.msgProducer.Start(s.ctx)
	// segment flush stream
	s.flushMsgStream, err = s.msFactory.NewMsgStream(s.ctx)
	if err != nil {
		return err
	}
	s.flushMsgStream.AsProducer([]string{Params.SegmentInfoChannelName})
	log.Debug("dataservice AsProducer:" + Params.SegmentInfoChannelName)
	s.flushMsgStream.Start()

	return nil
}

func (s *Server) loadMetaFromMaster() error {
	ctx := context.Background()
	log.Debug("loading collection meta from master")
	var err error
	if err = s.checkMasterIsHealthy(); err != nil {
		return err
	}
	if err = s.getDDChannel(); err != nil {
		return err
	}
	collections, err := s.masterClient.ShowCollections(ctx, &milvuspb.ShowCollectionsRequest{
		Base: &commonpb.MsgBase{
			MsgType:   commonpb.MsgType_ShowCollections,
			MsgID:     -1, // todo add msg id
			Timestamp: 0,  // todo
			SourceID:  Params.NodeID,
		},
		DbName: "",
	})
	if err = VerifyResponse(collections, err); err != nil {
		return err
	}
	for _, collectionName := range collections.CollectionNames {
		collection, err := s.masterClient.DescribeCollection(ctx, &milvuspb.DescribeCollectionRequest{
			Base: &commonpb.MsgBase{
				MsgType:   commonpb.MsgType_DescribeCollection,
				MsgID:     -1, // todo
				Timestamp: 0,  // todo
				SourceID:  Params.NodeID,
			},
			DbName:         "",
			CollectionName: collectionName,
		})
		if err = VerifyResponse(collection, err); err != nil {
			log.Error("describe collection error", zap.String("collectionName", collectionName), zap.Error(err))
			continue
		}
		partitions, err := s.masterClient.ShowPartitions(ctx, &milvuspb.ShowPartitionsRequest{
			Base: &commonpb.MsgBase{
				MsgType:   commonpb.MsgType_ShowPartitions,
				MsgID:     -1, // todo
				Timestamp: 0,  // todo
				SourceID:  Params.NodeID,
			},
			DbName:         "",
			CollectionName: collectionName,
			CollectionID:   collection.CollectionID,
		})
		if err = VerifyResponse(partitions, err); err != nil {
			log.Error("show partitions error", zap.String("collectionName", collectionName), zap.Int64("collectionID", collection.CollectionID), zap.Error(err))
			continue
		}
		err = s.meta.AddCollection(&datapb.CollectionInfo{
			ID:         collection.CollectionID,
			Schema:     collection.Schema,
			Partitions: partitions.PartitionIDs,
		})
		if err != nil {
			log.Error("add collection to meta error", zap.Int64("collectionID", collection.CollectionID), zap.Error(err))
			continue
		}
	}
	log.Debug("load collection meta from master complete")
	return nil
}

func (s *Server) getDDChannel() error {
	s.ddChannelMu.Lock()
	defer s.ddChannelMu.Unlock()
	if len(s.ddChannelMu.name) == 0 {
		resp, err := s.masterClient.GetDdChannel(s.ctx)
		if err = VerifyResponse(resp, err); err != nil {
			return err
		}
		s.ddChannelMu.name = resp.Value
	}
	return nil
}

func (s *Server) checkMasterIsHealthy() error {
	ticker := time.NewTicker(300 * time.Millisecond)
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer func() {
		ticker.Stop()
		cancel()
	}()
	for {
		var resp *internalpb.ComponentStates
		var err error
		select {
		case <-ctx.Done():
			return errors.New("master is not healthy")
		case <-ticker.C:
			resp, err = s.masterClient.GetComponentStates(ctx)
			if err = VerifyResponse(resp, err); err != nil {
				return err
			}
		}
		if resp.State.StateCode == internalpb.StateCode_Healthy {
			break
		}
	}
	return nil
}

func (s *Server) startServerLoop() {
	s.serverLoopCtx, s.serverLoopCancel = context.WithCancel(s.ctx)
	s.serverLoopWg.Add(2)
	go s.startStatsChannel(s.serverLoopCtx)
	go s.startSegmentFlushChannel(s.serverLoopCtx)
}

func (s *Server) startStatsChannel(ctx context.Context) {
	defer logutil.LogPanic()
	defer s.serverLoopWg.Done()
	statsStream, _ := s.msFactory.NewMsgStream(ctx)
	statsStream.AsConsumer([]string{Params.StatisticsChannelName}, Params.DataServiceSubscriptionName)
	log.Debug("dataservice AsConsumer: " + Params.StatisticsChannelName + " : " + Params.DataServiceSubscriptionName)
	// try to restore last processed pos
	pos, err := s.loadStreamLastPos(streamTypeStats)
	if err == nil {
		err = statsStream.Seek(pos)
		if err != nil {
			log.Error("Failed to seek to last pos for statsStream",
				zap.String("StatisChanName", Params.StatisticsChannelName),
				zap.String("DataServiceSubscriptionName", Params.DataServiceSubscriptionName),
				zap.Error(err))
		}
	}
	statsStream.Start()
	defer statsStream.Close()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		msgPack := statsStream.Consume()
		if msgPack == nil {
			continue
		}
		for _, msg := range msgPack.Msgs {
			if msg.Type() != commonpb.MsgType_SegmentStatistics {
				log.Warn("receive unknown msg from segment statistics channel", zap.Stringer("msgType", msg.Type()))
				continue
			}
			ssMsg := msg.(*msgstream.SegmentStatisticsMsg)
			for _, stat := range ssMsg.SegStats {
				if err := s.statsHandler.HandleSegmentStat(stat); err != nil {
					log.Error("handle segment stat error", zap.Int64("segmentID", stat.SegmentID), zap.Error(err))
					continue
				}
			}
			if ssMsg.MsgPosition != nil {
				err := s.storeStreamPos(streamTypeStats, ssMsg.MsgPosition)
				if err != nil {
					log.Error("Fail to store current success pos for Stats stream",
						zap.Stringer("pos", ssMsg.MsgPosition),
						zap.Error(err))
				}
			} else {
				log.Warn("Empty Msg Pos found ", zap.Int64("msgid", msg.ID()))
			}
		}
	}
}

func (s *Server) startSegmentFlushChannel(ctx context.Context) {
	defer logutil.LogPanic()
	defer s.serverLoopWg.Done()
	flushStream, _ := s.msFactory.NewMsgStream(ctx)
	flushStream.AsConsumer([]string{Params.SegmentInfoChannelName}, Params.DataServiceSubscriptionName)
	log.Debug("dataservice AsConsumer: " + Params.SegmentInfoChannelName + " : " + Params.DataServiceSubscriptionName)

	// try to restore last processed pos
	pos, err := s.loadStreamLastPos(streamTypeFlush)
	if err == nil {
		err = flushStream.Seek(pos)
		if err != nil {
			log.Error("Failed to seek to last pos for segment flush Stream",
				zap.String("SegInfoChannelName", Params.SegmentInfoChannelName),
				zap.String("DataServiceSubscriptionName", Params.DataServiceSubscriptionName),
				zap.Error(err))
		}
	}

	flushStream.Start()
	defer flushStream.Close()
	for {
		select {
		case <-ctx.Done():
			log.Debug("segment flush channel shut down")
			return
		default:
		}
		msgPack := flushStream.Consume()
		if msgPack == nil {
			continue
		}
		for _, msg := range msgPack.Msgs {
			if msg.Type() != commonpb.MsgType_SegmentFlushDone {
				log.Warn("receive unknown msg from segment flush channel", zap.Stringer("msgType", msg.Type()))
				continue
			}
			fcMsg := msg.(*msgstream.FlushCompletedMsg)
			err := s.meta.FlushSegment(fcMsg.SegmentID, fcMsg.BeginTimestamp)
			log.Debug("dataservice flushed segment", zap.Any("segmentID", fcMsg.SegmentID), zap.Error(err))
			if err != nil {
				log.Error("get segment from meta error", zap.Int64("segmentID", fcMsg.SegmentID), zap.Error(err))
				continue
			}

			if fcMsg.MsgPosition != nil {
				err = s.storeStreamPos(streamTypeFlush, fcMsg.MsgPosition)
				if err != nil {
					log.Error("Fail to store current success pos for segment flush stream",
						zap.Stringer("pos", fcMsg.MsgPosition),
						zap.Error(err))
				}
			} else {
				log.Warn("Empty Msg Pos found ", zap.Int64("msgid", msg.ID()))
			}
		}
	}
}

func (s *Server) Stop() error {
	s.cluster.ShutDownClients()
	s.ttMsgStream.Close()
	s.k2sMsgStream.Close()
	s.ttBarrier.Close()
	s.msgProducer.Close()
	s.stopServerLoop()
	return nil
}

// CleanMeta only for test
func (s *Server) CleanMeta() error {
	return s.kvClient.RemoveWithPrefix("")
}

func (s *Server) stopServerLoop() {
	s.serverLoopCancel()
	s.serverLoopWg.Wait()
}

func (s *Server) GetComponentStates(ctx context.Context) (*internalpb.ComponentStates, error) {
	resp := &internalpb.ComponentStates{
		State: &internalpb.ComponentInfo{
			NodeID:    Params.NodeID,
			Role:      role,
			StateCode: s.state.Load().(internalpb.StateCode),
		},
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}
	dataNodeStates, err := s.cluster.GetDataNodeStates(ctx)
	if err != nil {
		resp.Status.Reason = err.Error()
		return resp, nil
	}
	resp.SubcomponentStates = dataNodeStates
	resp.Status.ErrorCode = commonpb.ErrorCode_Success
	return resp, nil
}

func (s *Server) GetTimeTickChannel(ctx context.Context) (*milvuspb.StringResponse, error) {
	return &milvuspb.StringResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		},
		Value: Params.TimeTickChannelName,
	}, nil
}

func (s *Server) GetStatisticsChannel(ctx context.Context) (*milvuspb.StringResponse, error) {
	return &milvuspb.StringResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		},
		Value: Params.StatisticsChannelName,
	}, nil
}

func (s *Server) RegisterNode(ctx context.Context, req *datapb.RegisterNodeRequest) (*datapb.RegisterNodeResponse, error) {
	ret := &datapb.RegisterNodeResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}
	log.Debug("DataService: RegisterNode:", zap.String("IP", req.Address.Ip), zap.Int64("Port", req.Address.Port))
	node, err := s.newDataNode(req.Address.Ip, req.Address.Port, req.Base.SourceID)
	if err != nil {
		ret.Status.Reason = err.Error()
		return ret, nil
	}

	resp, err := node.client.WatchDmChannels(s.ctx, &datapb.WatchDmChannelsRequest{
		Base: &commonpb.MsgBase{
			MsgType:   0,
			MsgID:     0,
			Timestamp: 0,
			SourceID:  Params.NodeID,
		},
		ChannelNames: s.insertChannels,
	})

	if err = VerifyResponse(resp, err); err != nil {
		ret.Status.Reason = err.Error()
		return ret, nil
	}

	if err := s.getDDChannel(); err != nil {
		ret.Status.Reason = err.Error()
		return ret, nil
	}

	if s.ttBarrier != nil {
		if err = s.ttBarrier.AddPeer(node.id); err != nil {
			ret.Status.Reason = err.Error()
			return ret, nil
		}
	}

	if err = s.cluster.Register(node); err != nil {
		ret.Status.Reason = err.Error()
		return ret, nil
	}

	ret.Status.ErrorCode = commonpb.ErrorCode_Success
	ret.InitParams = &internalpb.InitParams{
		NodeID: Params.NodeID,
		StartParams: []*commonpb.KeyValuePair{
			{Key: "DDChannelName", Value: s.ddChannelMu.name},
			{Key: "SegmentStatisticsChannelName", Value: Params.StatisticsChannelName},
			{Key: "TimeTickChannelName", Value: Params.TimeTickChannelName},
			{Key: "CompleteFlushChannelName", Value: Params.SegmentInfoChannelName},
		},
	}
	return ret, nil
}

func (s *Server) newDataNode(ip string, port int64, id UniqueID) (*dataNode, error) {
	client := s.createDataNodeClient(fmt.Sprintf("%s:%d", ip, port))
	if err := client.Init(); err != nil {
		return nil, err
	}

	if err := client.Start(); err != nil {
		return nil, err
	}
	return &dataNode{
		id: id,
		address: struct {
			ip   string
			port int64
		}{ip: ip, port: port},
		client:     client,
		channelNum: 0,
	}, nil
}

func (s *Server) Flush(ctx context.Context, req *datapb.FlushRequest) (*commonpb.Status, error) {
	if !s.checkStateIsHealthy() {
		return &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    "server is initializing",
		}, nil
	}
	if err := s.segAllocator.SealAllSegments(ctx, req.CollectionID); err != nil {
		return &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    fmt.Sprintf("Seal all segments error %s", err),
		}, nil
	}
	return &commonpb.Status{
		ErrorCode: commonpb.ErrorCode_Success,
	}, nil
}

func (s *Server) AssignSegmentID(ctx context.Context, req *datapb.AssignSegmentIDRequest) (*datapb.AssignSegmentIDResponse, error) {
	if !s.checkStateIsHealthy() {
		return &datapb.AssignSegmentIDResponse{
			Status: &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
			},
		}, nil
	}

	assigns := make([]*datapb.SegmentIDAssignment, 0, len(req.SegmentIDRequests))

	var appendFailedAssignment = func(err string) {
		assigns = append(assigns, &datapb.SegmentIDAssignment{
			Status: &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
				Reason:    err,
			},
		})
	}

	for _, r := range req.SegmentIDRequests {
		if !s.meta.HasCollection(r.CollectionID) {
			if err := s.loadCollectionFromMaster(ctx, r.CollectionID); err != nil {
				appendFailedAssignment(fmt.Sprintf("can not load collection %d", r.CollectionID))
				log.Error("load collection from master error", zap.Int64("collectionID", r.CollectionID), zap.Error(err))
				continue
			}
		}
		//if err := s.validateAllocRequest(r.CollectionID, r.PartitionID, r.ChannelName); err != nil {
		//result.Status.Reason = err.Error()
		//assigns = append(assigns, result)
		//continue
		//}

		segmentID, retCount, expireTs, err := s.segAllocator.AllocSegment(ctx, r.CollectionID, r.PartitionID, r.ChannelName, int64(r.Count))
		if err != nil {
			appendFailedAssignment(fmt.Sprintf("allocation of collection %d, partition %d, channel %s, count %d error:  %s",
				r.CollectionID, r.PartitionID, r.ChannelName, r.Count, err.Error()))
			continue
		}

		result := &datapb.SegmentIDAssignment{
			SegID:        segmentID,
			ChannelName:  r.ChannelName,
			Count:        uint32(retCount),
			CollectionID: r.CollectionID,
			PartitionID:  r.PartitionID,
			ExpireTime:   expireTs,
			Status: &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_Success,
				Reason:    "",
			},
		}
		assigns = append(assigns, result)
	}
	return &datapb.AssignSegmentIDResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		},
		SegIDAssignments: assigns,
	}, nil
}

//func (s *Server) validateAllocRequest(collID UniqueID, partID UniqueID, channelName string) error {
//	if !s.meta.HasCollection(collID) {
//		return fmt.Errorf("can not find collection %d", collID)
//	}
//	if !s.meta.HasPartition(collID, partID) {
//		return fmt.Errorf("can not find partition %d", partID)
//	}
//	for _, name := range s.insertChannels {
//		if name == channelName {
//			return nil
//		}
//	}
//	return fmt.Errorf("can not find channel %s", channelName)
//}

func (s *Server) loadCollectionFromMaster(ctx context.Context, collectionID int64) error {
	resp, err := s.masterClient.DescribeCollection(ctx, &milvuspb.DescribeCollectionRequest{
		Base: &commonpb.MsgBase{
			MsgType:  commonpb.MsgType_DescribeCollection,
			SourceID: Params.NodeID,
		},
		DbName:       "",
		CollectionID: collectionID,
	})
	if err = VerifyResponse(resp, err); err != nil {
		return err
	}
	presp, err := s.masterClient.ShowPartitions(ctx, &milvuspb.ShowPartitionsRequest{
		Base: &commonpb.MsgBase{
			MsgType:   commonpb.MsgType_ShowPartitions,
			MsgID:     -1, // todo
			Timestamp: 0,  // todo
			SourceID:  Params.NodeID,
		},
		DbName:         "",
		CollectionName: resp.Schema.Name,
		CollectionID:   resp.CollectionID,
	})
	if err = VerifyResponse(presp, err); err != nil {
		log.Error("show partitions error", zap.String("collectionName", resp.Schema.Name), zap.Int64("collectionID", resp.CollectionID), zap.Error(err))
		return err
	}
	collInfo := &datapb.CollectionInfo{
		ID:         resp.CollectionID,
		Schema:     resp.Schema,
		Partitions: presp.PartitionIDs,
	}
	return s.meta.AddCollection(collInfo)
}

func (s *Server) ShowSegments(ctx context.Context, req *datapb.ShowSegmentsRequest) (*datapb.ShowSegmentsResponse, error) {
	resp := &datapb.ShowSegmentsResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}
	if !s.checkStateIsHealthy() {
		resp.Status.Reason = "server is initializing"
		return resp, nil
	}
	ids := s.meta.GetSegmentsOfPartition(req.CollectionID, req.PartitionID)
	resp.Status.ErrorCode = commonpb.ErrorCode_Success
	resp.SegmentIDs = ids
	return resp, nil
}

func (s *Server) GetSegmentStates(ctx context.Context, req *datapb.GetSegmentStatesRequest) (*datapb.GetSegmentStatesResponse, error) {
	resp := &datapb.GetSegmentStatesResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}
	if !s.checkStateIsHealthy() {
		resp.Status.Reason = "server is initializing"
		return resp, nil
	}

	for _, segmentID := range req.SegmentIDs {
		state := &datapb.SegmentStateInfo{
			Status:    &commonpb.Status{},
			SegmentID: segmentID,
		}
		segmentInfo, err := s.meta.GetSegment(segmentID)
		if err != nil {
			state.Status.ErrorCode = commonpb.ErrorCode_UnexpectedError
			state.Status.Reason = "get segment states error: " + err.Error()
		} else {
			state.Status.ErrorCode = commonpb.ErrorCode_Success
			state.State = segmentInfo.State
			state.StartPosition = segmentInfo.StartPosition
			state.EndPosition = segmentInfo.EndPosition
		}
		resp.States = append(resp.States, state)
	}
	resp.Status.ErrorCode = commonpb.ErrorCode_Success

	return resp, nil
}

func (s *Server) GetInsertBinlogPaths(ctx context.Context, req *datapb.GetInsertBinlogPathsRequest) (*datapb.GetInsertBinlogPathsResponse, error) {
	resp := &datapb.GetInsertBinlogPathsResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}
	p := path.Join(Params.SegmentFlushMetaPath, strconv.FormatInt(req.SegmentID, 10))
	_, values, err := s.kvClient.LoadWithPrefix(p)
	if err != nil {
		resp.Status.Reason = err.Error()
		return resp, nil
	}
	m := make(map[int64][]string)
	tMeta := &datapb.SegmentFieldBinlogMeta{}
	for _, v := range values {
		if err := proto.UnmarshalText(v, tMeta); err != nil {
			resp.Status.Reason = fmt.Errorf("DataService GetInsertBinlogPaths UnmarshalText datapb.SegmentFieldBinlogMeta err:%w", err).Error()
			return resp, nil
		}
		m[tMeta.FieldID] = append(m[tMeta.FieldID], tMeta.BinlogPath)
	}

	fids := make([]UniqueID, len(m))
	paths := make([]*internalpb.StringList, len(m))
	for k, v := range m {
		fids = append(fids, k)
		paths = append(paths, &internalpb.StringList{Values: v})
	}
	resp.Status.ErrorCode = commonpb.ErrorCode_Success
	resp.FieldIDs = fids
	resp.Paths = paths
	return resp, nil
}

func (s *Server) GetInsertChannels(ctx context.Context, req *datapb.GetInsertChannelsRequest) (*internalpb.StringList, error) {
	return &internalpb.StringList{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		},
		Values: s.insertChannels,
	}, nil
}

func (s *Server) GetCollectionStatistics(ctx context.Context, req *datapb.GetCollectionStatisticsRequest) (*datapb.GetCollectionStatisticsResponse, error) {
	resp := &datapb.GetCollectionStatisticsResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}
	nums, err := s.meta.GetNumRowsOfCollection(req.CollectionID)
	if err != nil {
		resp.Status.Reason = err.Error()
		return resp, nil
	}
	resp.Status.ErrorCode = commonpb.ErrorCode_Success
	resp.Stats = append(resp.Stats, &commonpb.KeyValuePair{Key: "row_count", Value: strconv.FormatInt(nums, 10)})
	return resp, nil
}

func (s *Server) GetPartitionStatistics(ctx context.Context, req *datapb.GetPartitionStatisticsRequest) (*datapb.GetPartitionStatisticsResponse, error) {
	resp := &datapb.GetPartitionStatisticsResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}
	nums, err := s.meta.GetNumRowsOfPartition(req.CollectionID, req.PartitionID)
	if err != nil {
		resp.Status.Reason = err.Error()
		return resp, nil
	}
	resp.Status.ErrorCode = commonpb.ErrorCode_Success
	resp.Stats = append(resp.Stats, &commonpb.KeyValuePair{Key: "row_count", Value: strconv.FormatInt(nums, 10)})
	return resp, nil
}

func (s *Server) GetSegmentInfoChannel(ctx context.Context) (*milvuspb.StringResponse, error) {
	return &milvuspb.StringResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		},
		Value: Params.SegmentInfoChannelName,
	}, nil
}

func (s *Server) GetSegmentInfo(ctx context.Context, req *datapb.GetSegmentInfoRequest) (*datapb.GetSegmentInfoResponse, error) {
	resp := &datapb.GetSegmentInfoResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}
	if !s.checkStateIsHealthy() {
		resp.Status.Reason = "data service is not healthy"
		return resp, nil
	}
	infos := make([]*datapb.SegmentInfo, 0, len(req.SegmentIDs))
	for _, id := range req.SegmentIDs {
		info, err := s.meta.GetSegment(id)
		if err != nil {
			resp.Status.Reason = err.Error()
			return resp, nil
		}
		infos = append(infos, info)
	}
	resp.Status.ErrorCode = commonpb.ErrorCode_Success
	resp.Infos = infos
	return resp, nil
}

// SaveBinlogPaths implement DataServiceServer
func (s *Server) SaveBinlogPaths(ctx context.Context, req *datapb.SaveBinlogPathsRequest) (*commonpb.Status, error) {
	resp := &commonpb.Status{
		ErrorCode: commonpb.ErrorCode_UnexpectedError,
	}
	if !s.checkStateIsHealthy() {
		resp.Reason = "server is initializing"
		return resp, nil
	}
	if s.flushMsgStream == nil {
		resp.Reason = "flush msg stream nil"
		return resp, nil
	}

	// check segment id & collection id matched
	_, err := s.meta.GetCollection(req.GetCollectionID())
	if err != nil {
		log.Error("Failed to get collection info", zap.Int64("collectionID", req.GetCollectionID()), zap.Error(err))
		resp.Reason = err.Error()
		return resp, err
	}

	segInfo, err := s.meta.GetSegment(req.GetSegmentID())
	if err != nil {
		log.Error("Failed to get segment info", zap.Int64("segmentID", req.GetSegmentID()), zap.Error(err))
		resp.Reason = err.Error()
		return resp, err
	}
	log.Debug("segment", zap.Int64("segment", segInfo.CollectionID))

	meta := make(map[string]string)
	fieldMeta, err := s.prepareField2PathMeta(req.SegmentID, req.Field2BinlogPaths)
	if err != nil {
		resp.Reason = err.Error()
		return resp, err
	}
	for k, v := range fieldMeta {
		meta[k] = v
	}
	ddlMeta, err := s.prepareDDLBinlogMeta(req.CollectionID, req.GetDdlBinlogPaths())
	if err != nil {
		resp.Reason = err.Error()
		return resp, err
	}
	for k, v := range ddlMeta {
		meta[k] = v
	}
	segmentPos, err := s.prepareSegmentPos(req.SegmentID, req.GetDmlPosition(), req.GetDdlPosition())
	if err != nil {
		resp.Reason = err.Error()
		return resp, err
	}
	for k, v := range segmentPos {
		meta[k] = v
	}
	// Save into k-v store
	err = s.SaveBinLogMetaTxn(meta)
	if err != nil {
		resp.Reason = err.Error()
		return resp, err
	}
	// write flush msg into segmentInfo/flush stream
	msgPack := composeSegmentFlushMsgPack(req.SegmentID)
	err = s.flushMsgStream.Produce(&msgPack)
	if err != nil {
		resp.Reason = err.Error()
		return resp, err
	}

	resp.ErrorCode = commonpb.ErrorCode_Success
	return resp, nil
}

func composeSegmentFlushMsgPack(segmentID UniqueID) msgstream.MsgPack {
	msgPack := msgstream.MsgPack{
		Msgs: make([]msgstream.TsMsg, 0, 1),
	}
	completeFlushMsg := internalpb.SegmentFlushCompletedMsg{
		Base: &commonpb.MsgBase{
			MsgType:   commonpb.MsgType_SegmentFlushDone,
			MsgID:     0, // TODO
			Timestamp: 0, // TODO
			SourceID:  Params.NodeID,
		},
		SegmentID: segmentID,
	}
	var msg msgstream.TsMsg = &msgstream.FlushCompletedMsg{
		BaseMsg: msgstream.BaseMsg{
			HashValues: []uint32{0},
		},
		SegmentFlushCompletedMsg: completeFlushMsg,
	}

	msgPack.Msgs = append(msgPack.Msgs, msg)
	return msgPack
}
