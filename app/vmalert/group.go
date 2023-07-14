package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/VictoriaMetrics/metrics"

	"github.com/VictoriaMetrics/VictoriaMetrics/app/vmalert/config"
	"github.com/VictoriaMetrics/VictoriaMetrics/app/vmalert/datasource"
	"github.com/VictoriaMetrics/VictoriaMetrics/app/vmalert/notifier"
	"github.com/VictoriaMetrics/VictoriaMetrics/app/vmalert/remotewrite"
	"github.com/VictoriaMetrics/VictoriaMetrics/app/vmalert/utils"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/decimal"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/prompbmarshal"
)

// Group is an entity for grouping rules
type Group struct {
	mu             sync.RWMutex
	Name           string
	File           string
	Rules          []Rule
	Type           config.Type
	Interval       time.Duration
	Limit          int
	Concurrency    int
	Checksum       string
	LastEvaluation time.Time

	Labels          map[string]string
	Params          url.Values
	Headers         map[string]string
	NotifierHeaders map[string]string

	doneCh     chan struct{}
	finishedCh chan struct{}
	// channel accepts new Group obj
	// which supposed to update current group
	updateCh chan *Group
	// evalCancel stores the cancel fn for interrupting
	// rules evaluation. Used on groups update() and close().
	evalCancel context.CancelFunc

	metrics *groupMetrics
}

type groupMetrics struct {
	iterationTotal    *utils.Counter
	iterationDuration *utils.Summary
	iterationMissed   *utils.Counter
	iterationInterval *utils.Gauge
}

func newGroupMetrics(g *Group) *groupMetrics {
	m := &groupMetrics{}
	labels := fmt.Sprintf(`group=%q, file=%q`, g.Name, g.File)
	m.iterationTotal = utils.GetOrCreateCounter(fmt.Sprintf(`vmalert_iteration_total{%s}`, labels))
	m.iterationDuration = utils.GetOrCreateSummary(fmt.Sprintf(`vmalert_iteration_duration_seconds{%s}`, labels))
	m.iterationMissed = utils.GetOrCreateCounter(fmt.Sprintf(`vmalert_iteration_missed_total{%s}`, labels))
	m.iterationInterval = utils.GetOrCreateGauge(fmt.Sprintf(`vmalert_iteration_interval_seconds{%s}`, labels), func() float64 {
		g.mu.RLock()
		i := g.Interval.Seconds()
		g.mu.RUnlock()
		return i
	})
	return m
}

// merges group rule labels into result map
// set2 has priority over set1.
func mergeLabels(groupName, ruleName string, set1, set2 map[string]string) map[string]string {
	r := map[string]string{}
	for k, v := range set1 {
		r[k] = v
	}
	for k, v := range set2 {
		if prevV, ok := r[k]; ok {
			logger.Infof("label %q=%q for rule %q.%q overwritten with external label %q=%q",
				k, prevV, groupName, ruleName, k, v)
		}
		r[k] = v
	}
	return r
}

func newGroup(cfg config.Group, qb datasource.QuerierBuilder, defaultInterval time.Duration, labels map[string]string) *Group {
	g := &Group{
		Type:            cfg.Type,
		Name:            cfg.Name,
		File:            cfg.File,
		Interval:        cfg.Interval.Duration(),
		Limit:           cfg.Limit,
		Concurrency:     cfg.Concurrency,
		Checksum:        cfg.Checksum,
		Params:          cfg.Params,
		Headers:         make(map[string]string),
		NotifierHeaders: make(map[string]string),
		Labels:          cfg.Labels,

		doneCh:     make(chan struct{}),
		finishedCh: make(chan struct{}),
		updateCh:   make(chan *Group),
	}
	if g.Interval == 0 {
		g.Interval = defaultInterval
	}
	if g.Concurrency < 1 {
		g.Concurrency = 1
	}
	for _, h := range cfg.Headers {
		g.Headers[h.Key] = h.Value
	}
	for _, h := range cfg.NotifierHeaders {
		g.NotifierHeaders[h.Key] = h.Value
	}
	g.metrics = newGroupMetrics(g)
	rules := make([]Rule, len(cfg.Rules))
	for i, r := range cfg.Rules {
		var extraLabels map[string]string
		// apply external labels
		if len(labels) > 0 {
			extraLabels = labels
		}
		// apply group labels, it has priority on external labels
		if len(cfg.Labels) > 0 {
			extraLabels = mergeLabels(g.Name, r.Name(), extraLabels, g.Labels)
		}
		// apply rules labels, it has priority on other labels
		if len(extraLabels) > 0 {
			r.Labels = mergeLabels(g.Name, r.Name(), extraLabels, r.Labels)
		}

		rules[i] = g.newRule(qb, r)
	}
	g.Rules = rules
	return g
}

func (g *Group) newRule(qb datasource.QuerierBuilder, rule config.Rule) Rule {
	if rule.Alert != "" {
		return newAlertingRule(qb, g, rule)
	}
	return newRecordingRule(qb, g, rule)
}

// ID return unique group ID that consists of
// rules file and group Name
func (g *Group) ID() uint64 {
	g.mu.RLock()
	defer g.mu.RUnlock()

	hash := fnv.New64a()
	hash.Write([]byte(g.File))
	hash.Write([]byte("\xff"))
	hash.Write([]byte(g.Name))
	hash.Write([]byte(g.Type.Get()))
	hash.Write([]byte(g.Interval.String()))
	return hash.Sum64()
}

// Restore restores alerts state for group rules
func (g *Group) Restore(ctx context.Context, qb datasource.QuerierBuilder, ts time.Time, lookback time.Duration) error {
	for _, rule := range g.Rules {
		ar, ok := rule.(*AlertingRule)
		if !ok {
			continue
		}
		if ar.For < 1 {
			continue
		}
		q := qb.BuildWithParams(datasource.QuerierParams{
			DataSourceType:     g.Type.String(),
			EvaluationInterval: g.Interval,
			QueryParams:        g.Params,
			Headers:            g.Headers,
			Debug:              ar.Debug,
		})
		if err := ar.Restore(ctx, q, ts, lookback); err != nil {
			return fmt.Errorf("error while restoring rule %q: %w", rule, err)
		}
	}
	return nil
}

// updateWith updates existing group with
// passed group object. This function ignores group
// evaluation interval change. It supposed to be updated
// in group.start function.
// Not thread-safe.
func (g *Group) updateWith(newGroup *Group) error {
	rulesRegistry := make(map[uint64]Rule)
	for _, nr := range newGroup.Rules {
		rulesRegistry[nr.ID()] = nr
	}

	for i, or := range g.Rules {
		nr, ok := rulesRegistry[or.ID()]
		if !ok {
			// old rule is not present in the new list
			// so we mark it for removing
			g.Rules[i].Close()
			g.Rules[i] = nil
			continue
		}
		if err := or.UpdateWith(nr); err != nil {
			return err
		}
		delete(rulesRegistry, nr.ID())
	}

	var newRules []Rule
	for _, r := range g.Rules {
		if r == nil {
			// skip nil rules
			continue
		}
		newRules = append(newRules, r)
	}
	// add the rest of rules from registry
	for _, nr := range rulesRegistry {
		newRules = append(newRules, nr)
	}
	// note that g.Interval is not updated here
	// so the value can be compared later in
	// group.Start function
	g.Type = newGroup.Type
	g.Concurrency = newGroup.Concurrency
	g.Params = newGroup.Params
	g.Headers = newGroup.Headers
	g.NotifierHeaders = newGroup.NotifierHeaders
	g.Labels = newGroup.Labels
	g.Limit = newGroup.Limit
	g.Checksum = newGroup.Checksum
	g.Rules = newRules
	return nil
}

// interruptEval interrupts in-flight rules evaluations
// within the group. It is expected that g.evalCancel
// will be repopulated after the call.
func (g *Group) interruptEval() {
	g.mu.RLock()
	defer g.mu.RUnlock()

	if g.evalCancel != nil {
		g.evalCancel()
	}
}

func (g *Group) close() {
	if g.doneCh == nil {
		return
	}
	close(g.doneCh)
	g.interruptEval()
	<-g.finishedCh

	g.metrics.iterationDuration.Unregister()
	g.metrics.iterationTotal.Unregister()
	g.metrics.iterationMissed.Unregister()
	g.metrics.iterationInterval.Unregister()
	for _, rule := range g.Rules {
		rule.Close()
	}
}

var skipRandSleepOnGroupStart bool

func (g *Group) start(ctx context.Context, nts func() []notifier.Notifier, rw *remotewrite.Client, rr datasource.QuerierBuilder) {
	defer func() { close(g.finishedCh) }()

	// Spread group rules evaluation over time in order to reduce load on VictoriaMetrics.
	if !skipRandSleepOnGroupStart {
		randSleep := uint64(float64(g.Interval) * (float64(g.ID()) / (1 << 64)))
		sleepOffset := uint64(time.Now().UnixNano()) % uint64(g.Interval)
		if randSleep < sleepOffset {
			randSleep += uint64(g.Interval)
		}
		randSleep -= sleepOffset
		sleepTimer := time.NewTimer(time.Duration(randSleep))
		select {
		case <-ctx.Done():
			sleepTimer.Stop()
			return
		case <-g.doneCh:
			sleepTimer.Stop()
			return
		case <-sleepTimer.C:
		}
	}

	e := &executor{
		rw:                       rw,
		notifiers:                nts,
		notifierHeaders:          g.NotifierHeaders,
		previouslySentSeriesToRW: make(map[uint64]map[string][]prompbmarshal.Label),
	}

	evalTS := time.Now()

	logger.Infof("group %q started; interval=%v; concurrency=%d", g.Name, g.Interval, g.Concurrency)

	eval := func(ctx context.Context, ts time.Time) {
		g.metrics.iterationTotal.Inc()

		start := time.Now()

		if len(g.Rules) < 1 {
			g.metrics.iterationDuration.UpdateDuration(start)
			g.LastEvaluation = start
			return
		}

		resolveDuration := getResolveDuration(g.Interval, *resendDelay, *maxResolveDuration)
		errs := e.execConcurrently(ctx, g.Rules, ts, g.Concurrency, resolveDuration, g.Limit)
		for err := range errs {
			if err != nil {
				logger.Errorf("group %q: %s", g.Name, err)
			}
		}
		g.metrics.iterationDuration.UpdateDuration(start)
		g.LastEvaluation = start
	}

	evalCtx, cancel := context.WithCancel(ctx)
	g.mu.Lock()
	g.evalCancel = cancel
	g.mu.Unlock()
	defer g.evalCancel()

	eval(evalCtx, evalTS)

	t := time.NewTicker(g.Interval)
	defer t.Stop()

	// restore the rules state after the first evaluation
	// so only active alerts can be restored.
	if rr != nil {
		err := g.Restore(ctx, rr, evalTS, *remoteReadLookBack)
		if err != nil {
			logger.Errorf("error while restoring ruleState for group %q: %s", g.Name, err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			logger.Infof("group %q: context cancelled", g.Name)
			return
		case <-g.doneCh:
			logger.Infof("group %q: received stop signal", g.Name)
			return
		case ng := <-g.updateCh:
			g.mu.Lock()

			// it is expected that g.evalCancel will be evoked
			// somewhere else to unblock group from the rules evaluation.
			// we recreate the evalCtx and g.evalCancel, so it can
			// be called again.
			evalCtx, cancel = context.WithCancel(ctx)
			g.evalCancel = cancel

			err := g.updateWith(ng)
			if err != nil {
				logger.Errorf("group %q: failed to update: %s", g.Name, err)
				g.mu.Unlock()
				continue
			}

			// ensure that staleness is tracked or existing rules only
			e.purgeStaleSeries(g.Rules)

			e.notifierHeaders = g.NotifierHeaders
			g.mu.Unlock()
			logger.Infof("group %q re-started; interval=%v; concurrency=%d", g.Name, g.Interval, g.Concurrency)
		case <-t.C:
			missed := (time.Since(evalTS) / g.Interval) - 1
			if missed < 0 {
				// missed can become < 0 due to irregular delays during evaluation
				// which can result in time.Since(evalTS) < g.Interval
				missed = 0
			}
			if missed > 0 {
				g.metrics.iterationMissed.Inc()
			}
			evalTS = evalTS.Add((missed + 1) * g.Interval)

			eval(evalCtx, evalTS)
		}
	}
}

// getResolveDuration returns the duration after which firing alert
// can be considered as resolved.
func getResolveDuration(groupInterval, delta, maxDuration time.Duration) time.Duration {
	if groupInterval > delta {
		delta = groupInterval
	}
	resolveDuration := delta * 4
	if maxDuration > 0 && resolveDuration > maxDuration {
		resolveDuration = maxDuration
	}
	return resolveDuration
}

type executor struct {
	notifiers       func() []notifier.Notifier
	notifierHeaders map[string]string

	rw *remotewrite.Client

	previouslySentSeriesToRWMu sync.Mutex
	// previouslySentSeriesToRW stores series sent to RW on previous iteration
	// map[ruleID]map[ruleLabels][]prompb.Label
	// where `ruleID` is ID of the Rule within a Group
	// and `ruleLabels` is []prompb.Label marshalled to a string
	previouslySentSeriesToRW map[uint64]map[string][]prompbmarshal.Label
}

func (e *executor) execConcurrently(ctx context.Context, rules []Rule, ts time.Time, concurrency int, resolveDuration time.Duration, limit int) chan error {
	res := make(chan error, len(rules))
	if concurrency == 1 {
		// fast path
		for _, rule := range rules {
			res <- e.exec(ctx, rule, ts, resolveDuration, limit)
		}
		close(res)
		return res
	}

	sem := make(chan struct{}, concurrency)
	go func() {
		wg := sync.WaitGroup{}
		for _, rule := range rules {
			sem <- struct{}{}
			wg.Add(1)
			go func(r Rule) {
				res <- e.exec(ctx, r, ts, resolveDuration, limit)
				<-sem
				wg.Done()
			}(rule)
		}
		wg.Wait()
		close(res)
	}()
	return res
}

var (
	alertsFired = metrics.NewCounter(`vmalert_alerts_fired_total`)

	execTotal  = metrics.NewCounter(`vmalert_execution_total`)
	execErrors = metrics.NewCounter(`vmalert_execution_errors_total`)

	remoteWriteErrors = metrics.NewCounter(`vmalert_remotewrite_errors_total`)
	remoteWriteTotal  = metrics.NewCounter(`vmalert_remotewrite_total`)
)

func (e *executor) exec(ctx context.Context, rule Rule, ts time.Time, resolveDuration time.Duration, limit int) error {
	execTotal.Inc()

	tss, err := rule.Exec(ctx, ts, limit)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// the context can be cancelled on graceful shutdown
			// or on group update. So no need to handle the error as usual.
			return nil
		}
		execErrors.Inc()
		return fmt.Errorf("rule %q: failed to execute: %w", rule, err)
	}

	if e.rw != nil {
		pushToRW := func(tss []prompbmarshal.TimeSeries) error {
			var lastErr error
			for _, ts := range tss {
				remoteWriteTotal.Inc()
				if err := e.rw.Push(ts); err != nil {
					remoteWriteErrors.Inc()
					lastErr = fmt.Errorf("rule %q: remote write failure: %w", rule, err)
				}
			}
			return lastErr
		}
		if err := pushToRW(tss); err != nil {
			return err
		}

		staleSeries := e.getStaleSeries(rule, tss, ts)
		if err := pushToRW(staleSeries); err != nil {
			return err
		}
	}

	ar, ok := rule.(*AlertingRule)
	if !ok {
		return nil
	}

	alerts := ar.alertsToSend(ts, resolveDuration, *resendDelay)
	if len(alerts) < 1 {
		return nil
	}

	wg := sync.WaitGroup{}
	errGr := new(utils.ErrGroup)
	for _, nt := range e.notifiers() {
		wg.Add(1)
		go func(nt notifier.Notifier) {
			if err := nt.Send(ctx, alerts, e.notifierHeaders); err != nil {
				errGr.Add(fmt.Errorf("rule %q: failed to send alerts to addr %q: %w", rule, nt.Addr(), err))
			}
			wg.Done()
		}(nt)
	}
	wg.Wait()
	return errGr.Err()
}

// getStaledSeries checks whether there are stale series from previously sent ones.
func (e *executor) getStaleSeries(rule Rule, tss []prompbmarshal.TimeSeries, timestamp time.Time) []prompbmarshal.TimeSeries {
	ruleLabels := make(map[string][]prompbmarshal.Label, len(tss))
	for _, ts := range tss {
		// convert labels to strings so we can compare with previously sent series
		key := labelsToString(ts.Labels)
		ruleLabels[key] = ts.Labels
	}

	rID := rule.ID()
	var staleS []prompbmarshal.TimeSeries
	// check whether there are series which disappeared and need to be marked as stale
	e.previouslySentSeriesToRWMu.Lock()
	for key, labels := range e.previouslySentSeriesToRW[rID] {
		if _, ok := ruleLabels[key]; ok {
			continue
		}
		// previously sent series are missing in current series, so we mark them as stale
		ss := newTimeSeriesPB([]float64{decimal.StaleNaN}, []int64{timestamp.Unix()}, labels)
		staleS = append(staleS, ss)
	}
	// set previous series to current
	e.previouslySentSeriesToRW[rID] = ruleLabels
	e.previouslySentSeriesToRWMu.Unlock()

	return staleS
}

// purgeStaleSeries deletes references in tracked
// previouslySentSeriesToRW list to Rules which aren't present
// in the given activeRules list. The method is used when the list
// of loaded rules has changed and executor has to remove
// references to non-existing rules.
func (e *executor) purgeStaleSeries(activeRules []Rule) {
	newPreviouslySentSeriesToRW := make(map[uint64]map[string][]prompbmarshal.Label)

	e.previouslySentSeriesToRWMu.Lock()

	for _, rule := range activeRules {
		id := rule.ID()
		prev, ok := e.previouslySentSeriesToRW[id]
		if ok {
			// keep previous series for staleness detection
			newPreviouslySentSeriesToRW[id] = prev
		}
	}
	e.previouslySentSeriesToRW = nil
	e.previouslySentSeriesToRW = newPreviouslySentSeriesToRW

	e.previouslySentSeriesToRWMu.Unlock()
}

func labelsToString(labels []prompbmarshal.Label) string {
	var b strings.Builder
	b.WriteRune('{')
	for i, label := range labels {
		if len(label.Name) == 0 {
			b.WriteString("__name__")
		} else {
			b.WriteString(label.Name)
		}
		b.WriteRune('=')
		b.WriteString(strconv.Quote(label.Value))
		if i < len(labels)-1 {
			b.WriteRune(',')
		}
	}
	b.WriteRune('}')
	return b.String()
}
