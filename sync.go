package targetsync

import (
	"context"
	"time"

	"github.com/jacksontj/lane"
	"github.com/sirupsen/logrus"
)

// Syncer is the struct that uses the various interfaces to actually do the sync
// TODO: metrics
type Syncer struct {
	Config *SyncConfig
	Locker Locker
	Src    TargetSource
	Dst    TargetDestination
}

// Run is the main method for the syncer. This is responsible for calling
// runLeader when the lock is held
func (s *Syncer) Run(ctx context.Context) error {
	logrus.Debugf("Syncer creating lock: %v", s.Config.LockOptions)
	electedCh, err := s.Locker.Lock(ctx, &s.Config.LockOptions)
	if err != nil {
		return err
	}

	var leaderCtx context.Context
	var leaderCtxCancel context.CancelFunc

	for {
		select {
		case <-ctx.Done():
			if leaderCtxCancel != nil {
				leaderCtxCancel()
			}
			return ctx.Err()
		case elected := <-electedCh:
			if elected {
				leaderCtx, leaderCtxCancel = context.WithCancel(ctx)
				logrus.Infof("Lock acquired, starting leader actions")
				go s.runLeader(leaderCtx)
			} else {
				logrus.Infof("Lock lost, stopping leader actions")
				if leaderCtxCancel != nil {
					leaderCtxCancel()
				}
			}
		}
	}
}

// bgRemove is a background goroutine responsible for removing targets from the destination
// this exists to allow for a `RemoveDelay` on the removal of targets from the destination
// to avoid issues where a target is "flapping" in the source
func (s *Syncer) bgRemove(ctx context.Context, removeCh chan *Target, addCh chan *Target) {
	itemMap := make(map[string]*lane.Item)
	q := lane.NewPQueue(lane.MINPQ)

	defaultDuration := time.Hour

	t := time.NewTimer(defaultDuration)
	for {
		select {
		case <-ctx.Done():
			return
		case toRemove := <-removeCh:
			logrus.Debugf("Scheduling target for removal from destination in %v: %v", s.Config.RemoveDelay, toRemove)
			now := time.Now()
			removeUnixTime := now.Add(s.Config.RemoveDelay).Unix()
			if headItem, headAt := q.Head(); headItem == nil || removeUnixTime < headAt {
				if !t.Stop() {
					<-t.C
				}
				t.Reset(s.Config.RemoveDelay)
			}
			itemMap[toRemove.Key()] = q.Push(toRemove, removeUnixTime)
		case toAdd := <-addCh:
			key := toAdd.Key()
			if item, ok := itemMap[key]; ok {
				logrus.Debugf("Removing target from removal queue as it was re-added: %v", toAdd)
				q.Remove(item)
				delete(itemMap, key)
			}
		case <-t.C:
			// Check if there is an item at head, and if the time is past then
			// do the removal
			headItem, headUnixTime := q.Head()
			logrus.Debugf("Processing target removal: %v", headItem)
			if headItem != nil {
				now := time.Now()
				// If we where woken before something is ready, just reschedule
				if headUnixTime < now.Unix() {
					d := time.Unix(headUnixTime, 0).Sub(now)
					if !t.Stop() {
						<-t.C
					}
					t.Reset(d)
				} else {
					target := headItem.(*Target)
					if err := s.Dst.RemoveTargets(ctx, []*Target{target}); err == nil {
						logrus.Debugf("Target removal successful: %v", target)
						q.Pop()
						delete(itemMap, target.Key())
					}
				}

				// Now that we did our thing, we need to calculate the next wake up time
				t.Reset(time.Unix(headUnixTime, 0).Sub(now))
			}
		}
	}
}

// runLeader does the actual syncing from source to destination. This is called
// after the leader election has been done, there should only be one of these per
// unique destination running globally
func (s *Syncer) runLeader(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	removeCh := make(chan *Target, 100)
	addCh := make(chan *Target, 100)
	defer close(removeCh)
	defer close(addCh)
	go s.bgRemove(ctx, removeCh, addCh)

	// get state from source
	srcCh, err := s.Src.Subscribe(ctx)
	if err != nil {
		return err
	}

	// Wait for an update, if we get one sync it
	for {
		logrus.Debugf("Waiting for targets from source")
		var srcTargets []*Target
		select {
		case <-ctx.Done():
			return ctx.Err()
		case srcTargets = <-srcCh:
		}
		logrus.Debugf("Received targets from source: %v", srcTargets)

		// get current ones from dst
		dstTargets, err := s.Dst.GetTargets(ctx)
		if err != nil {
			return err
		}
		logrus.Debugf("Fetched targets from destination: %v", dstTargets)

		// TODO: compare ports and do something with them
		srcMap := make(map[string]*Target)
		for _, target := range srcTargets {
			srcMap[target.IP] = target
		}
		dstMap := make(map[string]*Target)
		for _, target := range dstTargets {
			dstMap[target.IP] = target
		}

		// Add hosts first
		hostsToAdd := make([]*Target, 0)
		for ip, target := range srcMap {
			if _, ok := dstMap[ip]; !ok {
				hostsToAdd = append(hostsToAdd, target)
				addCh <- target
			}
		}
		if len(hostsToAdd) > 0 {
			logrus.Debugf("Adding targets to destination: %v", hostsToAdd)
			if err := s.Dst.AddTargets(ctx, hostsToAdd); err != nil {
				return err
			}
		}

		// Remove hosts last
		for ip, target := range dstMap {
			if _, ok := srcMap[ip]; !ok {
				removeCh <- target
			}
		}
	}
}
