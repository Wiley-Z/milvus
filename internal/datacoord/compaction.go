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

package datacoord

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/samber/lo"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/pkg/log"
	"github.com/milvus-io/milvus/pkg/metrics"
	"github.com/milvus-io/milvus/pkg/util/tsoutil"
	"github.com/milvus-io/milvus/pkg/util/typeutil"
)

// TODO this num should be determined by resources of datanode, for now, we set to a fixed value for simple
// TODO we should split compaction into different priorities, small compaction helps to merge segment, large compaction helps to handle delta and expiration of large segments
const (
	tsTimeout = uint64(1)
)

type compactionPlanContext interface {
	start()
	stop()
	// execCompactionPlan start to execute plan and return immediately
	execCompactionPlan(signal *compactionSignal, plan *datapb.CompactionPlan) error
	// getCompaction return compaction task. If planId does not exist, return nil.
	getCompaction(planID int64) *compactionTask
	// updateCompaction set the compaction state to timeout or completed
	updateCompaction(ts Timestamp) error
	// isFull return true if the task pool is full
	isFull() bool
	// get compaction tasks by signal id
	getCompactionTasksBySignalID(signalID int64) []*compactionTask
}

type compactionTaskState int8

const (
	executing compactionTaskState = iota + 1
	pipelining
	completed
	failed
	timeout
)

var (
	errChannelNotWatched = errors.New("channel is not watched")
	errChannelInBuffer   = errors.New("channel is in buffer")
)

type compactionTask struct {
	triggerInfo *compactionSignal
	plan        *datapb.CompactionPlan
	state       compactionTaskState
	dataNodeID  int64
	result      *datapb.CompactionResult
}

func (t *compactionTask) shadowClone(opts ...compactionTaskOpt) *compactionTask {
	task := &compactionTask{
		triggerInfo: t.triggerInfo,
		plan:        t.plan,
		state:       t.state,
		dataNodeID:  t.dataNodeID,
	}
	for _, opt := range opts {
		opt(task)
	}
	return task
}

var _ compactionPlanContext = (*compactionPlanHandler)(nil)

type compactionPlanHandler struct {
	plans            map[int64]*compactionTask // planID -> task
	sessions         SessionManager
	meta             *meta
	chManager        *ChannelManager
	mu               sync.RWMutex
	executingTaskNum int
	allocator        allocator
	quit             chan struct{}
	wg               sync.WaitGroup
	flushCh          chan UniqueID
	parallelCh       map[int64]chan struct{}
}

func newCompactionPlanHandler(sessions SessionManager, cm *ChannelManager, meta *meta,
	allocator allocator, flush chan UniqueID,
) *compactionPlanHandler {
	return &compactionPlanHandler{
		plans:      make(map[int64]*compactionTask),
		chManager:  cm,
		meta:       meta,
		sessions:   sessions,
		allocator:  allocator,
		flushCh:    flush,
		parallelCh: make(map[int64]chan struct{}),
	}
}

func (c *compactionPlanHandler) start() {
	interval := Params.DataCoordCfg.CompactionCheckIntervalInSeconds.GetAsDuration(time.Second)
	c.quit = make(chan struct{})
	c.wg.Add(2)

	go func() {
		defer c.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-c.quit:
				log.Info("compaction handler quit")
				return
			case <-ticker.C:
				ts, err := c.GetCurrentTS()
				if err != nil {
					log.Warn("unable to get current timestamp", zap.Error(err))
					continue
				}
				_ = c.updateCompaction(ts)
			}
		}
	}()

	go func() {
		defer c.wg.Done()
		cleanTicker := time.NewTicker(30 * time.Minute)
		defer cleanTicker.Stop()
		for {
			select {
			case <-c.quit:
				log.Info("Compaction handler quit clean")
				return
			case <-cleanTicker.C:
				c.Clean()
			}
		}
	}()
}

func (c *compactionPlanHandler) Clean() {
	current := tsoutil.GetCurrentTime()
	c.mu.Lock()
	defer c.mu.Unlock()

	for id, task := range c.plans {
		if task.state == executing || task.state == pipelining {
			continue
		}
		// after timeout + 1h, the plan will be cleaned
		if c.isTimeout(current, task.plan.GetStartTime(), task.plan.GetTimeoutInSeconds()+60*60) {
			delete(c.plans, id)
		}
	}
}

func (c *compactionPlanHandler) GetCurrentTS() (Timestamp, error) {
	interval := Params.DataCoordCfg.CompactionRPCTimeout.GetAsDuration(time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), interval)
	defer cancel()
	ts, err := c.allocator.allocTimestamp(ctx)
	if err != nil {
		log.Warn("unable to alloc timestamp", zap.Error(err))
		return 0, err
	}
	return ts, nil
}

func (c *compactionPlanHandler) stop() {
	close(c.quit)
	c.wg.Wait()
}

func (c *compactionPlanHandler) updateTask(planID int64, opts ...compactionTaskOpt) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if plan, ok := c.plans[planID]; ok {
		c.plans[planID] = plan.shadowClone(opts...)
	}
}

// execCompactionPlan start to execute plan and return immediately
func (c *compactionPlanHandler) execCompactionPlan(signal *compactionSignal, plan *datapb.CompactionPlan) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	nodeID, err := c.chManager.FindWatcher(plan.GetChannel())
	if err != nil {
		log.Error("failed to find watcher", zap.Int64("planID", plan.GetPlanID()), zap.Error(err))
		return err
	}

	log := log.With(zap.Int64("planID", plan.GetPlanID()), zap.Int64("nodeID", nodeID))
	c.setSegmentsCompacting(plan, true)

	task := &compactionTask{
		triggerInfo: signal,
		plan:        plan,
		state:       pipelining,
		dataNodeID:  nodeID,
	}
	c.plans[plan.PlanID] = task
	c.executingTaskNum++

	go func() {
		log.Info("acquire queue")
		c.acquireQueue(nodeID)

		ts, err := c.allocator.allocTimestamp(context.TODO())
		if err != nil {
			log.Warn("Alloc start time for CompactionPlan failed", zap.Error(err))
			// update plan ts to TIMEOUT ts
			c.updateTask(plan.PlanID, setState(executing), setStartTime(tsTimeout))
			return
		}
		c.updateTask(plan.PlanID, setStartTime(ts))
		err = c.sessions.Compaction(nodeID, plan)
		c.updateTask(plan.PlanID, setState(executing))
		if err != nil {
			log.Warn("try to Compaction but DataNode rejected", zap.Error(err))
			// do nothing here, prevent double release, see issue#21014
			// release queue will be done in `updateCompaction`
			return
		}
		log.Info("start compaction")
	}()
	return nil
}

func (c *compactionPlanHandler) setSegmentsCompacting(plan *datapb.CompactionPlan, compacting bool) {
	for _, segmentBinlogs := range plan.GetSegmentBinlogs() {
		c.meta.SetSegmentCompacting(segmentBinlogs.GetSegmentID(), compacting)
	}
}

// complete a compaction task
// not threadsafe, only can be used internally
func (c *compactionPlanHandler) completeCompaction(result *datapb.CompactionResult) error {
	planID := result.PlanID
	if _, ok := c.plans[planID]; !ok {
		return fmt.Errorf("plan %d is not found", planID)
	}

	if c.plans[planID].state != executing {
		return fmt.Errorf("plan %d's state is %v", planID, c.plans[planID].state)
	}

	plan := c.plans[planID].plan
	switch plan.GetType() {
	case datapb.CompactionType_MergeCompaction, datapb.CompactionType_MixCompaction:
		if err := c.handleMergeCompactionResult(plan, result); err != nil {
			return err
		}
	default:
		return errors.New("unknown compaction type")
	}
	metrics.DataCoordCompactedSegmentSize.WithLabelValues().Observe(float64(getCompactedSegmentSize(result)))
	c.plans[planID] = c.plans[planID].shadowClone(setState(completed), setResult(result), cleanLogPath())
	c.executingTaskNum--
	if c.plans[planID].plan.GetType() == datapb.CompactionType_MergeCompaction ||
		c.plans[planID].plan.GetType() == datapb.CompactionType_MixCompaction {
		c.flushCh <- result.GetSegmentID()
	}
	// TODO: when to clean task list

	nodeID := c.plans[planID].dataNodeID
	c.releaseQueue(nodeID)
	return nil
}

func (c *compactionPlanHandler) handleMergeCompactionResult(plan *datapb.CompactionPlan, result *datapb.CompactionResult) error {
	// Also prepare metric updates.
	newSegment, metricMutation, err := c.meta.CompleteCompactionMutation(plan, result)
	if err != nil {
		return err
	}
	log := log.With(zap.Int64("planID", plan.GetPlanID()))

	nodeID := c.plans[plan.GetPlanID()].dataNodeID
	req := &datapb.SyncSegmentsRequest{
		PlanID:        plan.PlanID,
		CompactedTo:   newSegment.GetID(),
		CompactedFrom: newSegment.GetCompactionFrom(),
		NumOfRows:     newSegment.GetNumOfRows(),
		StatsLogs:     newSegment.GetStatslogs(),
	}

	log.Info("handleCompactionResult: syncing segments with node", zap.Int64("nodeID", nodeID))
	if err := c.sessions.SyncSegments(nodeID, req); err != nil {
		log.Warn("handleCompactionResult: fail to sync segments with node, reverting metastore",
			zap.Int64("nodeID", nodeID), zap.Error(err))
		return err
	}
	// Apply metrics after successful meta update.
	metricMutation.commit()

	log.Info("handleCompactionResult: success to handle merge compaction result")
	return nil
}

// getCompaction return compaction task. If planId does not exist, return nil.
func (c *compactionPlanHandler) getCompaction(planID int64) *compactionTask {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.plans[planID]
}

// expireCompaction set the compaction state to expired
func (c *compactionPlanHandler) updateCompaction(ts Timestamp) error {
	// Get executing executingTasks before GetCompactionState from DataNode to prevent false failure,
	//  for DC might add new task while GetCompactionState.
	executingTasks := c.getTasksByState(executing)
	timeoutTasks := c.getTasksByState(timeout)
	planStates := c.sessions.GetCompactionPlanResults()
	cachedPlans := []int64{}

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, task := range executingTasks {
		log := log.With(
			zap.Int64("planID", task.plan.PlanID),
			zap.Int64("nodeID", task.dataNodeID),
			zap.String("channel", task.plan.GetChannel()))
		planID := task.plan.PlanID
		cachedPlans = append(cachedPlans, planID)

		if nodePlan, ok := planStates[planID]; ok {
			planResult := nodePlan.B

			switch planResult.GetState() {
			case commonpb.CompactionState_Completed:
				log.Info("start to complete compaction")

				// channels are balanced to other nodes, yet the old datanode still have the compaction results
				// task.dataNodeID == planState.A, but
				// task.dataNodeID not match with channel
				// Mark this compaction as failure and skip processing the meta
				if !c.chManager.Match(task.dataNodeID, task.plan.GetChannel()) {
					// Sync segments without CompactionFrom segmentsIDs to make sure DN clear the task
					// without changing the meta
					log.Warn("compaction failed for channel nodeID not match")
					if err := c.sessions.SyncSegments(task.dataNodeID, &datapb.SyncSegmentsRequest{PlanID: planID}); err != nil {
						log.Warn("compaction failed to sync segments with node", zap.Error(err))
						continue
					}
					c.plans[planID] = c.plans[planID].shadowClone(setState(failed))
					c.setSegmentsCompacting(task.plan, false)
					c.executingTaskNum--
					c.releaseQueue(task.dataNodeID)
				}

				if err := c.completeCompaction(planResult.GetResult()); err != nil {
					log.Warn("fail to complete compaction", zap.Error(err))
				}
			case commonpb.CompactionState_Executing:
				if c.isTimeout(ts, task.plan.GetStartTime(), task.plan.GetTimeoutInSeconds()) {
					log.Warn("compaction timeout",
						zap.Int32("timeout in seconds", task.plan.GetTimeoutInSeconds()),
						zap.Uint64("startTime", task.plan.GetStartTime()),
						zap.Uint64("now", ts),
					)
					c.plans[planID] = c.plans[planID].shadowClone(setState(timeout))
				}
			}
		} else {
			log.Info("compaction failed")
			c.plans[planID] = c.plans[planID].shadowClone(setState(failed))
			c.setSegmentsCompacting(task.plan, false)
			c.executingTaskNum--
			c.releaseQueue(task.dataNodeID)
		}
	}

	// Timeout tasks will be timeout and failed in DataNode
	// need to wait for DataNode reporting failure and
	// clean the status.
	for _, task := range timeoutTasks {
		log := log.With(
			zap.Int64("planID", task.plan.PlanID),
			zap.Int64("nodeID", task.dataNodeID),
			zap.String("channel", task.plan.GetChannel()),
		)

		planID := task.plan.PlanID
		cachedPlans = append(cachedPlans, planID)
		if nodePlan, ok := planStates[task.plan.PlanID]; ok {
			if nodePlan.B.GetState() == commonpb.CompactionState_Executing {
				log.RatedInfo(1, "compaction timeout in DataCoord yet DataNode is still running")
			}
		} else {
			// compaction task in DC but not found in DN means the compactino plan has failed
			log.Info("compaction failed for timeout")
			c.plans[planID] = c.plans[planID].shadowClone(setState(failed))
			c.setSegmentsCompacting(task.plan, false)
			c.executingTaskNum--
			c.releaseQueue(task.dataNodeID)
		}
	}

	// Compaction plans in DN but not in DC are unknown plans, need to notify DN to clear it.
	// No locks needed, because no changes in DC memeory
	completedPlans := lo.PickBy(planStates, func(planID int64, planState *typeutil.Pair[int64, *datapb.CompactionStateResult]) bool {
		return planState.B.GetState() == commonpb.CompactionState_Completed
	})

	unknownPlansInWorker, _ := lo.Difference(lo.Keys(completedPlans), cachedPlans)
	for _, planID := range unknownPlansInWorker {
		if nodeInvalidPlan, ok := completedPlans[planID]; ok {
			nodeID := nodeInvalidPlan.A
			log := log.With(zap.Int64("planID", planID), zap.Int64("nodeID", nodeID))

			// Sync segments without CompactionFrom segmentsIDs to make sure DN clear the task
			// without changing the meta
			log.Info("compaction syncing unknown plan with node")
			if err := c.sessions.SyncSegments(nodeID, &datapb.SyncSegmentsRequest{PlanID: planID}); err != nil {
				log.Warn("compaction failed to sync segments with node", zap.Error(err))
				return err
			}
		}
	}
	return nil
}

func (c *compactionPlanHandler) isTimeout(now Timestamp, start Timestamp, timeout int32) bool {
	startTime, _ := tsoutil.ParseTS(start)
	ts, _ := tsoutil.ParseTS(now)
	return int32(ts.Sub(startTime).Seconds()) >= timeout
}

func (c *compactionPlanHandler) acquireQueue(nodeID int64) {
	c.mu.Lock()
	_, ok := c.parallelCh[nodeID]
	if !ok {
		c.parallelCh[nodeID] = make(chan struct{}, calculateParallel())
	}
	c.mu.Unlock()

	c.mu.RLock()
	ch := c.parallelCh[nodeID]
	c.mu.RUnlock()
	ch <- struct{}{}
}

func (c *compactionPlanHandler) releaseQueue(nodeID int64) {
	log.Info("try to release queue", zap.Int64("nodeID", nodeID))
	ch, ok := c.parallelCh[nodeID]
	if !ok {
		return
	}
	<-ch
}

// isFull return true if the task pool is full
func (c *compactionPlanHandler) isFull() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.executingTaskNum >= Params.DataCoordCfg.CompactionMaxParallelTasks.GetAsInt()
}

func (c *compactionPlanHandler) getTasksByState(state compactionTaskState) []*compactionTask {
	c.mu.RLock()
	defer c.mu.RUnlock()
	tasks := make([]*compactionTask, 0, len(c.plans))
	for _, plan := range c.plans {
		if plan.state == state {
			tasks = append(tasks, plan)
		}
	}
	return tasks
}

// get compaction tasks by signal id; if signalID == 0 return all tasks
func (c *compactionPlanHandler) getCompactionTasksBySignalID(signalID int64) []*compactionTask {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var tasks []*compactionTask
	for _, t := range c.plans {
		if signalID == 0 {
			tasks = append(tasks, t)
			continue
		}
		if t.triggerInfo.id != signalID {
			continue
		}
		tasks = append(tasks, t)
	}
	return tasks
}

type compactionTaskOpt func(task *compactionTask)

func setState(state compactionTaskState) compactionTaskOpt {
	return func(task *compactionTask) {
		task.state = state
	}
}

func setStartTime(startTime uint64) compactionTaskOpt {
	return func(task *compactionTask) {
		task.plan.StartTime = startTime
	}
}

func setResult(result *datapb.CompactionResult) compactionTaskOpt {
	return func(task *compactionTask) {
		task.result = result
	}
}

func cleanLogPath() compactionTaskOpt {
	return func(task *compactionTask) {
		if task.plan.GetSegmentBinlogs() != nil {
			for _, binlogs := range task.plan.GetSegmentBinlogs() {
				binlogs.FieldBinlogs = nil
				binlogs.Deltalogs = nil
				binlogs.Field2StatslogPaths = nil
			}
		}
		if task.result != nil {
			task.result.InsertLogs = nil
			task.result.Deltalogs = nil
			task.result.Field2StatslogPaths = nil
		}
	}
}

// 0.5*min(8, NumCPU/2)
func calculateParallel() int {
	// TODO after node memory management enabled, use this config as hard limit
	return Params.DataCoordCfg.CompactionWorkerParalleTasks.GetAsInt()
	//cores := hardware.GetCPUNum()
	//if cores < 16 {
	//return 4
	//}
	//return cores / 2
}
