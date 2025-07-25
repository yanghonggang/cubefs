// Copyright 2022 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package cluster

import (
	"container/heap"
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubefs/cubefs/blobstore/api/blobnode"
	"github.com/cubefs/cubefs/blobstore/api/clustermgr"
	"github.com/cubefs/cubefs/blobstore/api/shardnode"
	"github.com/cubefs/cubefs/blobstore/clustermgr/base"
	"github.com/cubefs/cubefs/blobstore/clustermgr/scopemgr"
	"github.com/cubefs/cubefs/blobstore/common/codemode"
	apierrors "github.com/cubefs/cubefs/blobstore/common/errors"
	"github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/blobstore/common/raftserver"
	"github.com/cubefs/cubefs/blobstore/common/trace"
	"github.com/cubefs/cubefs/blobstore/util/errors"
)

var (
	_ BlobNodeManagerAPI  = (*BlobNodeManager)(nil)
	_ ShardNodeManagerAPI = (*ShardNodeManager)(nil)
)

const (
	defaultRefreshIntervalS                = 300
	defaultHeartbeatExpireIntervalS        = 60
	defaultFlushIntervalS                  = 600
	defaultListDiskMaxCount                = 200
	defaultApplyConcurrency         uint32 = 10
)

// CopySet Config
const (
	ecNodeSetID   = proto.NodeSetID(1)
	ecDiskSetID   = proto.DiskSetID(1)
	nullNodeSetID = proto.NodeSetID(0)
	nullDiskSetID = proto.DiskSetID(0)
)

var (
	ErrDiskExist                  = errors.New("disk already exist")
	ErrDiskNotExist               = errors.New("disk not exist")
	ErrNoEnoughSpace              = errors.New("no enough space to alloc")
	ErrBlobNodeCreateChunkFailed  = errors.New("blob node create chunk failed")
	ErrShardNodeCreateShardFailed = errors.New("shard node create shard failed")
	ErrNodeExist                  = errors.New("node already exist")
	ErrNodeNotExist               = errors.New("node not exist")
)

var validSetStatus = map[proto.DiskStatus]int{
	proto.DiskStatusNormal:    0,
	proto.DiskStatusBroken:    1,
	proto.DiskStatusRepairing: 2,
	proto.DiskStatusRepaired:  3,
	proto.DiskStatusDropped:   4,
}

type NodeManagerAPI interface {
	// AllocNodeID return a unused node id
	AllocNodeID(ctx context.Context) (proto.NodeID, error)
	// AllocDiskID return a unused disk id
	AllocDiskID(ctx context.Context) (proto.DiskID, error)
	// CheckDiskInfoDuplicated return true if disk info already exit, like host and path duplicated
	CheckDiskInfoDuplicated(ctx context.Context, diskID proto.DiskID, info *clustermgr.DiskInfo, nodeInfo *clustermgr.NodeInfo) error
	// IsDiskWritable judge disk if writable, disk status unmoral or readonly or heartbeat timeout will return true
	IsDiskWritable(ctx context.Context, id proto.DiskID) (bool, error)
	// SetStatus change disk status, in some case, change status is not allow
	// like change repairing/repaired/dropped into normal
	SetStatus(ctx context.Context, id proto.DiskID, status proto.DiskStatus, isCommit bool) error
	// IsDroppingDisk return true if the specified disk is dropping
	IsDroppingDisk(ctx context.Context, id proto.DiskID) (bool, error)
	// Stat return disk statistic info of a cluster
	Stat(ctx context.Context, diskType proto.DiskType) *clustermgr.SpaceStatInfo
	// GetHeartbeatChangeDisks return any heartbeat change disks
	GetHeartbeatChangeDisks() []HeartbeatEvent
	// ValidateNodeInfo validate node info and return any validation error when validate fail
	ValidateNodeInfo(ctx context.Context, info *clustermgr.NodeInfo) error
	CheckNodeInfoDuplicated(ctx context.Context, info *clustermgr.NodeInfo) (proto.NodeID, bool)
	RefreshExpireTime()
}

type persistentHandler interface {
	updateDiskNoLocked(di *diskItem) error
	updateDiskStatusNoLocked(id proto.DiskID, status proto.DiskStatus) error
	addDiskNoLocked(di *diskItem) error
	updateNodeNoLocked(n *nodeItem) error
	addDroppingDisk(id proto.DiskID) error
	addDroppingNode(id proto.NodeID) error
	isDroppingDisk(id proto.DiskID) (bool, error)
	isDroppingNode(id proto.NodeID) (bool, error)
	droppedDisk(id proto.DiskID) error
	droppedNode(id proto.NodeID) error
}

//type Module struct {
//	blobNodeMgr  *BlobNodeManager
//	shardNodeMgr *ShardNodeManager
//}

type HeartbeatEvent struct {
	DiskID  proto.DiskID
	IsAlive bool
}

type DiskMgrConfig struct {
	RefreshIntervalS         int                 `json:"refresh_interval_s"`
	RackAware                bool                `json:"rack_aware"`
	HostAware                bool                `json:"host_aware"`
	HeartbeatExpireIntervalS int                 `json:"heartbeat_expire_interval_s"`
	FlushIntervalS           int                 `json:"flush_interval_s"`
	ApplyConcurrency         uint32              `json:"apply_concurrency"`
	BlobNodeConfig           blobnode.Config     `json:"blob_node_config"`
	ShardNodeConfig          shardnode.Config    `json:"shard_node_config"`
	AllocTolerateBuffer      int64               `json:"alloc_tolerate_buffer"`
	EnsureIndex              bool                `json:"ensure_index"`
	IDC                      []string            `json:"-"`
	CodeModes                []codemode.CodeMode `json:"-"`
	ChunkSize                int64               `json:"-"`
	ChunkOversoldRatio       float64             `json:"-"`
	ShardSize                int64               `json:"-"`
	DiskIDScopeName          string              `json:"-"`
	NodeIDScopeName          string              `json:"-"`

	CopySetConfigs map[proto.DiskType]CopySetConfig `json:"copy_set_configs"`
}

type CopySetConfig struct {
	NodeSetCap                int `json:"node_set_cap"`
	NodeSetRackCap            int `json:"node_set_rack_cap"`
	DiskSetCap                int `json:"disk_set_cap"`
	DiskCountPerNodeInDiskSet int `json:"disk_count_per_node_in_disk_set"`

	NodeSetIdcCap int `json:"-"`
}

type manager struct {
	module            string
	allDisks          map[proto.DiskID]*diskItem
	allNodes          map[proto.NodeID]*nodeItem
	topoMgr           *topoMgr
	allocator         atomic.Value
	taskPool          *base.TaskDistribution
	hostPathFilter    sync.Map
	pendingEntries    sync.Map
	raftServer        raftserver.RaftServer
	scopeMgr          scopemgr.ScopeMgrAPI
	persistentHandler persistentHandler

	lastFlushTime time.Time
	spaceStatInfo atomic.Value
	metaLock      sync.RWMutex
	closeCh       chan interface{}
	cfg           DiskMgrConfig
}

func (d *manager) Close() {
	close(d.closeCh)
	d.taskPool.Close()
}

func (d *manager) RefreshExpireTime() {
	// fast copy all diskItem pointer
	disks := d.getAllDisk()
	for _, di := range disks {
		di.withLocked(func() error {
			di.lastExpireTime = time.Now().Add(time.Duration(d.cfg.HeartbeatExpireIntervalS) * time.Second)
			di.expireTime = time.Now().Add(time.Duration(d.cfg.HeartbeatExpireIntervalS) * time.Second)
			return nil
		})
	}
}

func (d *manager) SetRaftServer(raftServer raftserver.RaftServer) {
	d.raftServer = raftServer
}

func (d *manager) AllocDiskID(ctx context.Context) (proto.DiskID, error) {
	_, diskID, err := d.scopeMgr.Alloc(ctx, d.cfg.DiskIDScopeName, 1)
	if err != nil {
		return 0, errors.Info(err, "diskMgr.AllocDiskID failed").Detail(err)
	}
	return proto.DiskID(diskID), nil
}

// IsFrequentHeartBeat judge disk heartbeat interval whether small than HeartbeatNotifyIntervalS
func (d *manager) IsFrequentHeartBeat(id proto.DiskID, HeartbeatNotifyIntervalS int) (bool, error) {
	diskInfo, ok := d.getDisk(id)
	if !ok {
		return false, apierrors.ErrCMDiskNotFound
	}
	diskInfo.lock.RLock()
	defer diskInfo.lock.RUnlock()

	newExpireTime := time.Now().Add(time.Duration(d.cfg.HeartbeatExpireIntervalS) * time.Second)
	if newExpireTime.Sub(diskInfo.expireTime) < time.Duration(HeartbeatNotifyIntervalS)*time.Second {
		return true, nil
	}
	return false, nil
}

func (d *manager) CheckDiskInfoDuplicated(ctx context.Context, diskID proto.DiskID, diskInfo *clustermgr.DiskInfo, nodeInfo *clustermgr.NodeInfo) error {
	span := trace.SpanFromContextSafe(ctx)
	di, ok := d.getDisk(diskID)
	// compatible case: disk register again to diskSet
	if ok && di.info.NodeID == proto.InvalidNodeID && diskInfo.NodeID != proto.InvalidNodeID &&
		di.info.Host == nodeInfo.Host && di.info.Idc == nodeInfo.Idc && di.info.Rack == nodeInfo.Rack {
		return nil
	}
	if ok { // disk exist
		span.Warn("disk exist")
		return apierrors.ErrExist
	}
	disk := &diskItem{
		info: diskItemInfo{DiskInfo: clustermgr.DiskInfo{Host: nodeInfo.Host, Path: diskInfo.Path}},
	}
	if _, ok = d.hostPathFilter.Load(disk.genFilterKey()); ok {
		span.Warn("host and path duplicated")
		return apierrors.ErrIllegalArguments
	}
	return nil
}

func (d *manager) IsDiskWritable(ctx context.Context, id proto.DiskID) (bool, error) {
	diskInfo, ok := d.getDisk(id)
	if !ok {
		return false, apierrors.ErrCMDiskNotFound
	}

	diskInfo.lock.RLock()
	defer diskInfo.lock.RUnlock()

	return diskInfo.isWritable(), nil
}

func (d *manager) SetStatus(ctx context.Context, id proto.DiskID, status proto.DiskStatus, isCommit bool) error {
	var (
		beforeSeq int
		afterSeq  int
		ok        bool
		span      = trace.SpanFromContextSafe(ctx)
	)

	if afterSeq, ok = validSetStatus[status]; !ok {
		return apierrors.ErrInvalidStatus
	}

	disk, ok := d.getDisk(id)
	if !ok {
		span.Error("diskMgr.SetStatus disk not found in all disks, diskID: %v, status: %v", id, status)
		return apierrors.ErrCMDiskNotFound
	}

	err := disk.withRLocked(func() error {
		if disk.info.Status == status {
			return nil
		}
		// disallow set disk status when disk is dropping, as disk status will be dropped finally
		if disk.dropping && status != proto.DiskStatusDropped {
			if !isCommit {
				return apierrors.ErrChangeDiskStatusNotAllow
			}
			span.Warnf("disk[%d] is dropping, can't set disk status", id)
			return nil
		}

		beforeSeq, ok = validSetStatus[disk.info.Status]
		if !ok {
			panic(fmt.Sprintf("invalid disk status in disk table, diskid: %d, state: %d", id, status))
		}
		// can't change status back or change status more than 2 motion
		if beforeSeq > afterSeq || (afterSeq-beforeSeq > 1 && status != proto.DiskStatusDropped) {
			// return error in pre set request
			if !isCommit {
				return apierrors.ErrChangeDiskStatusNotAllow
			}
			// return nil in wal log replay situation
			span.Warnf("disallow set disk[%d] status[%d], before seq: %d, after seq: %d", id, status, beforeSeq, afterSeq)
			return nil
		}

		return nil
	})
	if err != nil {
		return err
	}

	if !isCommit {
		return nil
	}

	// Call getNode outside disk lock, avoid nested meta and disk lock
	nodeID := proto.InvalidNodeID
	disk.withRLocked(func() error {
		nodeID = disk.info.NodeID
		return nil
	})
	node, nodeExist := d.getNode(nodeID)

	return disk.withLocked(func() error {
		// concurrent double check
		if disk.info.Status == status {
			return nil
		}
		var err error
		if status == proto.DiskStatusDropped {
			err = d.persistentHandler.droppedDisk(id)
		} else {
			err = d.persistentHandler.updateDiskStatusNoLocked(id, status)
		}
		if err != nil {
			err = errors.Info(err, "diskMgr.SetStatus update disk info failed").Detail(err)
			span.Error(errors.Detail(err))
			return err
		}
		disk.info.Status = status
		if !disk.needFilter() {
			d.hostPathFilter.Delete(disk.genFilterKey())
		}
		if nodeExist && !disk.needFilter() { // compatible case && diskRepaired
			d.topoMgr.RemoveDiskFromDiskSet(node.info.DiskType, node.info.NodeSetID, disk)
		}

		return nil
	})
}

func (d *manager) IsDroppingDisk(ctx context.Context, id proto.DiskID) (bool, error) {
	disk, ok := d.getDisk(id)
	if !ok {
		return false, apierrors.ErrCMDiskNotFound
	}
	disk.lock.RLock()
	defer disk.lock.RUnlock()
	if disk.dropping {
		return true, nil
	}
	return false, nil
}

// Stat return disk statistic info of a cluster
func (d *manager) Stat(ctx context.Context, diskType proto.DiskType) *clustermgr.SpaceStatInfo {
	spaceStatInfo := d.spaceStatInfo.Load().(map[proto.DiskType]*clustermgr.SpaceStatInfo)
	diskTypeInfo, ok := spaceStatInfo[diskType]
	if !ok {
		return &clustermgr.SpaceStatInfo{}
	}
	ret := *diskTypeInfo
	return &ret
}

// SwitchReadonly can switch disk's readonly or writable
func (d *manager) applySwitchReadonly(diskID proto.DiskID, readonly bool) error {
	disk, _ := d.getDisk(diskID)

	disk.lock.RLock()
	if disk.info.Readonly == readonly {
		disk.lock.RUnlock()
		return nil
	}
	disk.lock.RUnlock()

	disk.lock.Lock()
	defer disk.lock.Unlock()
	disk.info.Readonly = readonly
	err := d.persistentHandler.updateDiskNoLocked(disk)
	if err != nil {
		disk.info.Readonly = !readonly
		return err
	}
	return nil
}

func (d *manager) GetHeartbeatChangeDisks() []HeartbeatEvent {
	all := d.getAllDisk()
	ret := make([]HeartbeatEvent, 0)
	span := trace.SpanFromContextSafe(context.Background())
	for _, disk := range all {
		disk.lock.RLock()
		span.Debugf("diskId:%d,expireTime:%v,lastExpireTime:%v", disk.diskID, disk.expireTime, disk.lastExpireTime)
		// notify topper level when heartbeat expire or heartbeat recover
		if disk.isExpire() && disk.needFilter() {
			span.Warnf("diskId:%d was expired,expireTime:%v,lastExpireTime:%v", disk.diskID, disk.expireTime, disk.lastExpireTime)

			// expired disk has been notified already, then ignore it
			if time.Since(disk.expireTime) >= 2*time.Duration(d.cfg.HeartbeatExpireIntervalS)*time.Second {
				disk.lock.RUnlock()
				continue
			}
			ret = append(ret, HeartbeatEvent{DiskID: disk.diskID, IsAlive: false})
			disk.lock.RUnlock()
			continue
		}
		if disk.expireTime.Sub(disk.lastExpireTime) > 1*time.Duration(d.cfg.HeartbeatExpireIntervalS)*time.Second {
			ret = append(ret, HeartbeatEvent{DiskID: disk.diskID, IsAlive: true})
		}
		disk.lock.RUnlock()
	}

	return ret
}

func (d *manager) AllocNodeID(ctx context.Context) (proto.NodeID, error) {
	_, nodeID, err := d.scopeMgr.Alloc(ctx, d.cfg.NodeIDScopeName, 1)
	if err != nil {
		return 0, errors.Info(err, "diskMgr.AllocNodeID failed").Detail(err)
	}
	return proto.NodeID(nodeID), nil
}

func (d *manager) GetTopoInfo(ctx context.Context) *clustermgr.TopoInfo {
	ret := &clustermgr.TopoInfo{
		CurNodeSetID: d.topoMgr.GetNodeSetID(),
		CurDiskSetID: d.topoMgr.GetDiskSetID(),
		AllNodeSets:  make(map[string]map[proto.NodeSetID]*clustermgr.NodeSetInfo),
	}

	nodeSetsMap := d.topoMgr.GetAllNodeSets(ctx)
	for diskType, nodeSets := range nodeSetsMap {
		if _, ok := ret.AllNodeSets[diskType.String()]; !ok {
			ret.AllNodeSets[diskType.String()] = make(map[proto.NodeSetID]*clustermgr.NodeSetInfo)
		}
		for _, nodeSet := range nodeSets {
			nodeSetInfo, ok := ret.AllNodeSets[diskType.String()][nodeSet.ID()]
			if !ok {
				nodeSetInfo = &clustermgr.NodeSetInfo{
					ID:       nodeSet.ID(),
					Number:   nodeSet.GetNodeNum(),
					Nodes:    nodeSet.GetNodeIDs(),
					DiskSets: make(map[proto.DiskSetID][]proto.DiskID),
				}
				ret.AllNodeSets[diskType.String()][nodeSet.ID()] = nodeSetInfo
			}
			diskSets := nodeSet.GetDiskSets()
			for _, diskSet := range diskSets {
				nodeSetInfo.DiskSets[diskSet.ID()] = diskSet.GetDiskIDs()
			}
		}
	}
	return ret
}

func (d *manager) CheckNodeInfoDuplicated(ctx context.Context, info *clustermgr.NodeInfo) (proto.NodeID, bool) {
	node := &nodeItem{
		info: nodeItemInfo{NodeInfo: clustermgr.NodeInfo{Host: info.Host, DiskType: info.DiskType}},
	}
	if v, ok := d.hostPathFilter.Load(node.genFilterKey()); ok {
		nodeID := v.(proto.NodeID)
		return nodeID, true
	}
	return proto.InvalidNodeID, false
}

func (d *manager) ValidateNodeInfo(ctx context.Context, info *clustermgr.NodeInfo) error {
	if !info.Role.IsValid() {
		return apierrors.ErrIllegalArguments
	}
	if !info.DiskType.IsValid() {
		return apierrors.ErrIllegalArguments
	}
	if info.NodeSetID != nullNodeSetID {
		if err := d.topoMgr.ValidateNodeSetID(ctx, info.DiskType, info.NodeSetID); err != nil {
			return err
		}
	}

	return nil
}

// applyAddNode add a new node into cluster, it returns ErrNodeExist if node already exist
func (d *manager) applyAddNode(ctx context.Context, info interface{}) error {
	span := trace.SpanFromContextSafe(ctx)
	var nodeInfo clustermgr.NodeInfo
	shardNodeInfo, isShardNode := info.(*clustermgr.ShardNodeInfo)
	if isShardNode {
		nodeInfo = shardNodeInfo.NodeInfo
	}
	blobNodeInfo, isBlobNode := info.(*clustermgr.BlobNodeInfo)
	if isBlobNode {
		nodeInfo = blobNodeInfo.NodeInfo
	}

	// concurrent double check
	_, ok := d.getNode(nodeInfo.NodeID)
	if ok {
		return nil
	}

	// alloc NodeSetID
	if nodeInfo.NodeSetID == nullNodeSetID {
		nodeInfo.NodeSetID = d.topoMgr.AllocNodeSetID(ctx, &nodeInfo, d.cfg.CopySetConfigs[nodeInfo.DiskType], d.cfg.RackAware)
	}
	nodeInfo.Status = proto.NodeStatusNormal

	ni := &nodeItem{
		nodeID: nodeInfo.NodeID,
		info:   nodeItemInfo{NodeInfo: nodeInfo},
		disks:  make(map[proto.DiskID]*diskItem),
	}
	if isShardNode {
		ni.info.extraInfo = shardNodeInfo.ShardNodeExtraInfo
	}

	// add node to nodeTbl and nodeSet
	err := d.persistentHandler.updateNodeNoLocked(ni)
	if err != nil {
		span.Error("ShardNodeManager.addNode add node failed: ", err)
		return errors.Info(err, "ShardNodeManager.addNode add node failed").Detail(err)
	}

	d.topoMgr.AddNodeToNodeSet(ni)
	d.metaLock.Lock()
	d.allNodes[nodeInfo.NodeID] = ni
	d.metaLock.Unlock()
	d.hostPathFilter.Store(ni.genFilterKey(), ni.nodeID)

	return nil
}

// droppingDisk add a dropping disk
func (d *manager) applyDroppingDisk(ctx context.Context, id proto.DiskID, isCommit bool) (bool, error) {
	span := trace.SpanFromContextSafe(ctx)
	disk, ok := d.getDisk(id)
	if !ok {
		return false, apierrors.ErrCMDiskNotFound
	}

	var dropping bool
	disk.withRLocked(func() error {
		dropping = disk.dropping
		return nil
	})
	if dropping {
		return true, nil
	}

	err := disk.withRLocked(func() error {
		// only normal and readonly disk can add into dropping list
		if disk.info.Status != proto.DiskStatusNormal || !disk.info.Readonly {
			span.Warnf("disk[%d] status is not normal or readonly, can't add into dropping disk list", id)
			return apierrors.ErrDiskAbnormalOrNotReadOnly
		}
		return nil
	})
	if err != nil {
		if !isCommit {
			return false, err
		}
		// return err by pendingEntries in commit case
		pendingKey := fmtApplyContextKey("disk-dropping", id.ToString())
		if _, ok = d.pendingEntries.Load(pendingKey); ok {
			d.pendingEntries.Store(pendingKey, err)
		}
		return false, nil
	}
	if !isCommit {
		return false, nil
	}

	err = d.persistentHandler.addDroppingDisk(id)
	if err != nil {
		return false, err
	}

	// call getNode outside disk lock, avoid nested meta and disk lock
	nodeID := proto.InvalidNodeID
	disk.withLocked(func() error {
		disk.dropping = true
		nodeID = disk.info.NodeID
		return nil
	})
	// remove disk from diskSet on dropping disk, avoid the new expanded disk not being properly added to the diskSet when dropping node
	if node, ok := d.getNode(nodeID); ok { // compatible case
		d.topoMgr.RemoveDiskFromDiskSet(node.info.DiskType, node.info.NodeSetID, disk)
	}

	return false, nil
}

// droppedDisk set disk dropped
func (d *manager) applyDroppedDisk(ctx context.Context, id proto.DiskID) error {
	exist, err := d.persistentHandler.isDroppingDisk(id)
	if err != nil {
		return errors.Info(err, "diskMgr.droppedDisk get dropping disk failed").Detail(err)
	}
	// concurrent dropped request may cost dropping disk not found, don't return error in this situation
	if !exist {
		return nil
	}

	err = d.SetStatus(ctx, id, proto.DiskStatusDropped, true)
	if err != nil {
		err = errors.Info(err, "diskMgr.droppedDisk set disk dropped status failed").Detail(err)
	}

	disk, _ := d.getDisk(id)
	disk.lock.Lock()
	disk.dropping = false
	disk.lock.Unlock()

	return err
}

// applyDroppingNode add a dropping node
func (d *manager) applyDroppingNode(ctx context.Context, nodeID proto.NodeID, isCommit bool) (bool, error) {
	node, ok := d.getNode(nodeID)
	if !ok {
		return false, apierrors.ErrCMNodeNotFound
	}

	// check node status
	err := node.withRLocked(func() error {
		if !node.isUsingStatus() || node.dropping {
			return apierrors.ErrCMNodeIsDropping
		}
		return nil
	})
	if err != nil {
		return true, nil
	}

	// copy diskIDs of node, avoid nested node and disk lock
	var diskItems []*diskItem
	node.withRLocked(func() error {
		diskItems = make([]*diskItem, 0, len(node.disks))
		for _, di := range node.disks {
			diskItems = append(diskItems, di)
		}
		return nil
	})

	for _, di := range diskItems {
		err = di.withRLocked(func() error {
			if di.info.Status != proto.DiskStatusNormal {
				return apierrors.ErrDiskAbnormalOrNotReadOnly
			}
			return nil
		})
		// skip disk which is abnormal(dropped or repaired one is not in use, broken or repairing one will be set repaired finally)
		if err != nil {
			continue
		}
		_, err = d.applyDroppingDisk(ctx, di.diskID, isCommit)
		if err != nil {
			if !isCommit {
				return false, err
			}
			// return err by pendingEntries in commit case
			pendingKey := fmtApplyContextKey("node-dropping", nodeID.ToString())
			if _, ok = d.pendingEntries.Load(pendingKey); ok {
				d.pendingEntries.Store(pendingKey, err)
			}
			return false, nil
		}
	}
	if !isCommit {
		return false, nil
	}
	// dropping the node
	err = d.persistentHandler.addDroppingNode(nodeID)
	if err != nil {
		return false, err
	}
	node.withLocked(func() error {
		node.dropping = true
		return nil
	})

	return false, nil
}

// applyDroppedNode dropped a node
func (d *manager) applyDroppedNode(ctx context.Context, nodeID proto.NodeID) error {
	span := trace.SpanFromContextSafe(ctx)

	exist, err := d.persistentHandler.isDroppingNode(nodeID)
	if err != nil {
		return errors.Info(err, "applyDroppedNode get dropping node failed").Detail(err)
	}
	// concurrent request may cost dropping node not found, don't return error in this case
	if !exist {
		return nil
	}

	node, ok := d.getNode(nodeID)
	if !ok {
		return apierrors.ErrCMNodeNotFound
	}

	var diskItems []*diskItem
	node.withRLocked(func() error {
		// copy diskIDs of node, avoid nested node and disk lock
		diskItems = make([]*diskItem, 0, len(node.disks))
		for _, di := range node.disks {
			diskItems = append(diskItems, di)
		}
		return nil
	})
	// check disk status again
	for _, di := range diskItems {
		err = di.withRLocked(func() error {
			if di.needFilter() {
				return errors.New(fmt.Sprintf("node has disk[%d] in use", di.diskID))
			}
			return nil
		})
		if err != nil {
			span.Errorf("applyDroppedNode check disk status err: %v", err)
			return nil
		}
	}

	return node.withLocked(func() error {
		err = d.persistentHandler.droppedNode(node.nodeID)
		if err != nil {
			return errors.Info(err, "diskMgr.droppedNode dropped node failed").Detail(err)
		}
		node.info.Status = proto.NodeStatusDropped
		node.dropping = false
		d.topoMgr.RemoveNodeFromNodeSet(node)
		return nil
	})
}

func (d *manager) getDisk(diskID proto.DiskID) (disk *diskItem, exist bool) {
	d.metaLock.RLock()
	disk, exist = d.allDisks[diskID]
	d.metaLock.RUnlock()
	return
}

// getAllDisk copy all diskItem pointer array
func (d *manager) getAllDisk() []*diskItem {
	d.metaLock.RLock()
	total := len(d.allDisks)
	all := make([]*diskItem, 0, total)
	for _, disk := range d.allDisks {
		all = append(all, disk)
	}
	d.metaLock.RUnlock()
	return all
}

func (d *manager) getNode(nodeID proto.NodeID) (node *nodeItem, exist bool) {
	d.metaLock.RLock()
	node, exist = d.allNodes[nodeID]
	d.metaLock.RUnlock()
	return
}

func (d *manager) getDiskType(disk *diskItem) proto.DiskType {
	n, _ := d.getNode(disk.info.NodeID)
	if n == nil {
		// compatible
		return proto.DiskTypeHDD
	}
	return n.info.DiskType
}

func (d *manager) validateAllocRet(disks []proto.DiskID) error {
	if d.cfg.HostAware {
		selectedHost := make(map[string]bool)
		for i := range disks {
			disk, ok := d.getDisk(disks[i])
			if !ok {
				return errors.Info(ErrDiskNotExist, fmt.Sprintf("disk[%d]", disks[i])).Detail(ErrDiskNotExist)
			}
			disk.lock.RLock()
			if selectedHost[disk.info.Host] {
				disk.lock.RUnlock()
				return errors.New(fmt.Sprintf("duplicated host, selected disks: %v", disks))
			}
			selectedHost[disk.info.Host] = true
			disk.lock.RUnlock()
		}
		return nil
	}

	selectedDisk := make(map[proto.DiskID]bool)
	for i := range disks {
		if selectedDisk[disks[i]] {
			return errors.New(fmt.Sprintf("duplicated disk, selected disks: %v", disks))
		}
		selectedDisk[disks[i]] = true
	}

	return nil
}

func (d *manager) generateDiskSetStorage(ctx context.Context, disks []*diskItem, spaceStatInfo *clustermgr.SpaceStatInfo,
	diskStatInfosM map[string]*clustermgr.DiskStatInfo,
) (ret map[string]*idcAllocator, freeChunk int64) {
	span := trace.SpanFromContextSafe(ctx)
	nodeStgs := make(map[string]*nodeAllocator)
	idcFreeItems := make(map[string]int64)
	idcRackStgs := make(map[string]map[string]*rackAllocator)
	idcNodeStgs := make(map[string][]*nodeAllocator)
	rackNodeStgs := make(map[string][]*nodeAllocator)
	rackFreeItems := make(map[string]int64)

	var (
		free, size, diskFreeItem, diskMaxItem int64
		idc, rack, host                       string
	)
	for _, disk := range disks {
		// call getNode outside disk lock, avoid nested meta and disk lock
		nodeID := proto.InvalidNodeID
		disk.withRLocked(func() error {
			nodeID = disk.info.NodeID
			return nil
		})
		node, nodeExist := d.getNode(nodeID)
		// read one disk info
		err := disk.withRLocked(func() error {
			idc = disk.info.Idc
			rack = disk.info.Rack
			host = disk.info.Host
			if nodeExist {
				idc = node.info.Idc
				rack = node.info.Rack
				host = node.info.Host
			}
			// idc disk status num calculate
			if diskStatInfosM[idc] == nil {
				diskStatInfosM[idc] = &clustermgr.DiskStatInfo{IDC: idc}
			}
			blobNodeHeartbeatInfo, isBlobNodeDisk := disk.info.extraInfo.(*clustermgr.DiskHeartBeatInfo)
			if isBlobNodeDisk {
				free = blobNodeHeartbeatInfo.Free
				size = blobNodeHeartbeatInfo.Size
				diskFreeItem = blobNodeHeartbeatInfo.FreeChunkCnt
				originalDiskFreeItem, diskFreeItem := blobNodeHeartbeatInfo.FreeChunkCnt, blobNodeHeartbeatInfo.FreeChunkCnt
				if blobNodeHeartbeatInfo.OversoldFreeChunkCnt > diskFreeItem {
					diskFreeItem = blobNodeHeartbeatInfo.OversoldFreeChunkCnt
				}
				diskMaxItem = blobNodeHeartbeatInfo.MaxChunkCnt
				diskStatInfosM[idc].TotalFreeChunk += originalDiskFreeItem
				diskStatInfosM[idc].TotalOversoldFreeChunk += diskFreeItem
				diskStatInfosM[idc].TotalChunk += diskMaxItem
			}
			shardNodeHeartbeatInfo, isShardNodeDisk := disk.info.extraInfo.(*clustermgr.ShardNodeDiskHeartbeatInfo)
			if isShardNodeDisk {
				free = shardNodeHeartbeatInfo.Free
				size = shardNodeHeartbeatInfo.Size
				diskFreeItem = int64(shardNodeHeartbeatInfo.FreeShardCnt)
				diskMaxItem = int64(shardNodeHeartbeatInfo.MaxShardCnt)
				diskStatInfosM[idc].TotalFreeShard += diskFreeItem
				diskStatInfosM[idc].TotalShard += diskMaxItem
			}
			readonly := disk.info.Readonly
			status := disk.info.Status
			// rack can be the same in different idc, so we make rack string with idc
			rack = idc + "-" + rack
			spaceStatInfo.TotalDisk += 1
			diskStatInfosM[idc].Total += 1
			if readonly {
				diskStatInfosM[idc].Readonly += 1
			}
			switch status {
			case proto.DiskStatusBroken:
				diskStatInfosM[idc].Broken += 1
			case proto.DiskStatusRepairing:
				diskStatInfosM[idc].Repairing += 1
			case proto.DiskStatusRepaired:
				diskStatInfosM[idc].Repaired += 1
			case proto.DiskStatusDropped:
				diskStatInfosM[idc].Dropped += 1
			default:
			}
			if disk.dropping {
				diskStatInfosM[idc].Dropping += 1
			}
			// filter abnormal disk
			if disk.info.Status != proto.DiskStatusNormal {
				return errors.New("abnormal disk")
			}
			spaceStatInfo.TotalSpace += size
			if readonly { // include dropping disk
				spaceStatInfo.ReadOnlySpace += free
				return errors.New("readonly disk")
			}
			spaceStatInfo.FreeSpace += free
			diskStatInfosM[idc].Available += 1

			// filter expired disk
			if disk.isExpire() {
				diskStatInfosM[idc].Expired += 1
				return errors.New("expired disk")
			}

			return nil
		})
		if err != nil {
			span.Infof("This is %v, not to build allocator", err)
			continue
		}

		// build for idcRackStorage
		if _, ok := idcRackStgs[idc]; !ok {
			idcRackStgs[idc] = make(map[string]*rackAllocator)
		}
		if _, ok := idcRackStgs[idc][rack]; !ok {
			idcRackStgs[idc][rack] = &rackAllocator{rack: rack}
		}
		// build for idcAllocator
		if _, ok := idcNodeStgs[idc]; !ok {
			idcNodeStgs[idc] = make([]*nodeAllocator, 0)
			idcFreeItems[idc] = 0
		}
		idcFreeItems[idc] += diskFreeItem
		// build for rackAllocator
		if _, ok := rackNodeStgs[rack]; !ok {
			rackNodeStgs[rack] = make([]*nodeAllocator, 0)
			rackFreeItems[rack] = 0
		}
		rackFreeItems[rack] += diskFreeItem
		// build for nodeAllocator
		if _, ok := nodeStgs[host]; !ok {
			nodeStgs[host] = &nodeAllocator{host: host, disks: make([]*diskItem, 0)}
			// append idc data node
			idcNodeStgs[idc] = append(idcNodeStgs[idc], nodeStgs[host])
			// append rack data node
			rackNodeStgs[rack] = append(rackNodeStgs[rack], nodeStgs[host])
		}
		nodeStgs[host].disks = append(nodeStgs[host].disks, disk)
		nodeStgs[host].weight += diskFreeItem
		nodeStgs[host].free += free
	}

	span.Debugf("all nodeStgs: %+v", nodeStgs)
	for _, rackStgs := range idcRackStgs {
		for rack := range rackStgs {
			rackStgs[rack].weight = rackFreeItems[rack]
			rackStgs[rack].nodeStorages = rackNodeStgs[rack]
		}
	}
	for idc := range idcNodeStgs {
		span.Infof("%s idcNodeStgs length: %d", idc, len(idcNodeStgs[idc]))
	}

	spaceStatInfo.UsedSpace = spaceStatInfo.TotalSpace - spaceStatInfo.FreeSpace - spaceStatInfo.ReadOnlySpace

	if len(idcRackStgs) > 0 {
		ret = make(map[string]*idcAllocator)
		for i := range d.cfg.IDC {
			ret[d.cfg.IDC[i]] = &idcAllocator{
				idc:          d.cfg.IDC[i],
				weight:       idcFreeItems[d.cfg.IDC[i]],
				diffRack:     d.cfg.RackAware,
				diffHost:     d.cfg.HostAware,
				rackStorages: idcRackStgs[d.cfg.IDC[i]],
				nodeStorages: idcNodeStgs[d.cfg.IDC[i]],
			}
			freeChunk += idcFreeItems[d.cfg.IDC[i]]
		}
		spaceStatInfo.WritableSpace += d.calculateWritable(idcNodeStgs)
	}

	return
}

func (d *manager) calculateWritable(nodeStgs map[string][]*nodeAllocator) int64 {
	// writable space statistic
	codeMode, suCount := d.getMaxSuCount()
	idcSuCount := suCount / len(d.cfg.IDC)
	var itemSize int64
	if d.cfg.ChunkSize != 0 {
		itemSize = d.cfg.ChunkSize
	}
	if d.cfg.ShardSize != 0 {
		itemSize = d.cfg.ShardSize
	}

	if d.cfg.HostAware && len(nodeStgs) > 0 {
		// calculate minimum idc writable item num
		calIDCWritableFunc := func(stgs []*nodeAllocator) int64 {
			stripe := make([]int64, idcSuCount)
			lefts := make(maxHeap, 0)
			n := int64(0)
			for _, v := range stgs {
				count := v.free / itemSize
				if count > 0 {
					lefts = append(lefts, count)
				}
			}

			heap.Init(&lefts)
			for {
				if lefts.Len() < idcSuCount {
					break
				}
				for i := 0; i < idcSuCount; i++ {
					stripe[i] = heap.Pop(&lefts).(int64)
				}
				// set minimum stripe count to 10 with more random selection, optimize writable space accuracy
				min := int64(10)
				n += min
				for i := 0; i < idcSuCount; i++ {
					stripe[i] -= min
					if stripe[i] > 0 {
						heap.Push(&lefts, stripe[i])
					}
				}
			}
			return n
		}
		minimumStripeCount := int64(math.MaxInt64)
		for idc := range nodeStgs {
			n := calIDCWritableFunc(nodeStgs[idc])
			if n < minimumStripeCount {
				minimumStripeCount = n
			}
		}
		return minimumStripeCount * int64(codeMode.Tactic().N) * itemSize
	}

	if len(nodeStgs) > 0 {
		minimumChunkNum := int64(math.MaxInt64)
		for idc := range nodeStgs {
			idcChunkNum := int64(0)
			for i := range nodeStgs[idc] {
				idcChunkNum += nodeStgs[idc][i].free / itemSize
			}
			if idcChunkNum < minimumChunkNum {
				minimumChunkNum = idcChunkNum
			}
		}
		return minimumChunkNum / int64(idcSuCount) * int64(codeMode.Tactic().N) * itemSize
	}

	return 0
}

func (d *manager) getMaxSuCount() (codemode.CodeMode, int) {
	suCount := 0
	idx := 0
	for i := range d.cfg.CodeModes {
		codeModeInfo := d.cfg.CodeModes[i].Tactic()
		if codeModeInfo.N+codeModeInfo.M+codeModeInfo.L > suCount {
			idx = i
			suCount = codeModeInfo.N + codeModeInfo.M + codeModeInfo.L
		}
	}
	return d.cfg.CodeModes[idx], suCount
}

func fmtApplyContextKey(opType, id string) string {
	return fmt.Sprintf("%s-%s", opType, id)
}
