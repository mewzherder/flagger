package controller

import (
	"fmt"
	"github.com/weaveworks/flagger/pkg/router"
	"strings"
	"time"

	flaggerv1 "github.com/weaveworks/flagger/pkg/apis/flagger/v1alpha3"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
)

// scheduleCanaries synchronises the canary map with the jobs map,
// for new canaries new jobs are created and started
// for the removed canaries the jobs are stopped and deleted
func (c *Controller) scheduleCanaries() {
	current := make(map[string]string)
	stats := make(map[string]int)

	c.canaries.Range(func(key interface{}, value interface{}) bool {
		canary := value.(*flaggerv1.Canary)

		// format: <name>.<namespace>
		name := key.(string)
		current[name] = fmt.Sprintf("%s.%s", canary.Spec.TargetRef.Name, canary.Namespace)

		job, exists := c.jobs[name]
		// schedule new job for exsiting job with different analysisInterval or non-existing job
		if (exists && job.GetCanaryAnalysisInterval() != canary.GetAnalysisInterval()) || !exists {
			if exists {
				job.Stop()
			}

			newJob := CanaryJob{
				Name:             canary.Name,
				Namespace:        canary.Namespace,
				function:         c.advanceCanary,
				done:             make(chan bool),
				ticker:           time.NewTicker(canary.GetAnalysisInterval()),
				analysisInterval: canary.GetAnalysisInterval(),
			}

			c.jobs[name] = newJob
			newJob.Start()
		}

		// compute canaries per namespace total
		t, ok := stats[canary.Namespace]
		if !ok {
			stats[canary.Namespace] = 1
		} else {
			stats[canary.Namespace] = t + 1
		}
		return true
	})

	// cleanup deleted jobs
	for job := range c.jobs {
		if _, exists := current[job]; !exists {
			c.jobs[job].Stop()
			delete(c.jobs, job)
		}
	}

	// check if multiple canaries have the same target
	for canaryName, targetName := range current {
		for name, target := range current {
			if name != canaryName && target == targetName {
				c.logger.With("canary", canaryName).Errorf("Bad things will happen! Found more than one canary with the same target %s", targetName)
			}
		}
	}

	// set total canaries per namespace metric
	for k, v := range stats {
		c.recorder.SetTotal(k, v)
	}
}

func (c *Controller) advanceCanary(name string, namespace string, skipLivenessChecks bool) {
	begin := time.Now()
	// check if the canary exists
	cd, err := c.flaggerClient.FlaggerV1alpha3().Canaries(namespace).Get(name, v1.GetOptions{})
	if err != nil {
		c.logger.With("canary", fmt.Sprintf("%s.%s", name, namespace)).
			Errorf("Canary %s.%s not found", name, namespace)
		return
	}

	primaryName := fmt.Sprintf("%s-primary", cd.Spec.TargetRef.Name)

	// create primary deployment and hpa if needed
	if err := c.deployer.Sync(cd); err != nil {
		c.recordEventWarningf(cd, "%v", err)
		return
	}

	// init routers
	routerFactory := router.NewFactory(c.kubeClient, c.flaggerClient, c.logger, c.istioClient)
	meshRouter := routerFactory.MeshRouter(c.meshProvider)

	// create ClusterIP services and virtual service if needed
	if err := routerFactory.KubernetesRouter().Sync(cd); err != nil {
		c.recordEventWarningf(cd, "%v", err)
		return
	}

	// create or update virtual service
	if err := meshRouter.Sync(cd); err != nil {
		c.recordEventWarningf(cd, "%v", err)
		return
	}

	shouldAdvance, err := c.deployer.ShouldAdvance(cd)
	if err != nil {
		c.recordEventWarningf(cd, "%v", err)
		return
	}

	if !shouldAdvance {
		return
	}

	// set max weight default value to 100%
	maxWeight := 100
	if cd.Spec.CanaryAnalysis.MaxWeight > 0 {
		maxWeight = cd.Spec.CanaryAnalysis.MaxWeight
	}

	// check primary deployment status
	if !skipLivenessChecks {
		if _, err := c.deployer.IsPrimaryReady(cd); err != nil {
			c.recordEventWarningf(cd, "%v", err)
			return
		}
	}

	// check if virtual service exists
	// and if it contains weighted destination routes to the primary and canary services
	primaryWeight, canaryWeight, err := meshRouter.GetRoutes(cd)
	if err != nil {
		c.recordEventWarningf(cd, "%v", err)
		return
	}

	c.recorder.SetWeight(cd, primaryWeight, canaryWeight)

	// check if canary analysis should start (canary revision has changes) or continue
	if ok := c.checkCanaryStatus(cd, shouldAdvance); !ok {
		return
	}

	// check if canary revision changed during analysis
	if restart := c.hasCanaryRevisionChanged(cd); restart {
		c.recordEventInfof(cd, "New revision detected! Restarting analysis for %s.%s",
			cd.Spec.TargetRef.Name, cd.Namespace)

		// route all traffic back to primary
		primaryWeight = 100
		canaryWeight = 0
		if err := meshRouter.SetRoutes(cd, primaryWeight, canaryWeight); err != nil {
			c.recordEventWarningf(cd, "%v", err)
			return
		}

		// reset status
		status := flaggerv1.CanaryStatus{
			Phase:        flaggerv1.CanaryProgressing,
			CanaryWeight: 0,
			FailedChecks: 0,
			Iterations:   0,
		}
		if err := c.deployer.SyncStatus(cd, status); err != nil {
			c.recordEventWarningf(cd, "%v", err)
			return
		}
		return
	}

	defer func() {
		c.recorder.SetDuration(cd, time.Since(begin))
	}()

	// check canary deployment status
	var retriable = true
	if !skipLivenessChecks {
		retriable, err = c.deployer.IsCanaryReady(cd)
		if err != nil && retriable {
			c.recordEventWarningf(cd, "%v", err)
			return
		}
	}

	// check if analysis should be skipped
	if skip := c.shouldSkipAnalysis(cd, meshRouter, primaryWeight, canaryWeight); skip {
		return
	}

	// check if the number of failed checks reached the threshold
	if cd.Status.Phase == flaggerv1.CanaryProgressing &&
		(!retriable || cd.Status.FailedChecks >= cd.Spec.CanaryAnalysis.Threshold) {

		if cd.Status.FailedChecks >= cd.Spec.CanaryAnalysis.Threshold {
			c.recordEventWarningf(cd, "Rolling back %s.%s failed checks threshold reached %v",
				cd.Name, cd.Namespace, cd.Status.FailedChecks)
			c.sendNotification(cd, fmt.Sprintf("Failed checks threshold reached %v", cd.Status.FailedChecks),
				false, true)
		}

		if !retriable {
			c.recordEventWarningf(cd, "Rolling back %s.%s progress deadline exceeded %v",
				cd.Name, cd.Namespace, err)
			c.sendNotification(cd, fmt.Sprintf("Progress deadline exceeded %v", err),
				false, true)
		}

		// route all traffic back to primary
		primaryWeight = 100
		canaryWeight = 0
		if err := meshRouter.SetRoutes(cd, primaryWeight, canaryWeight); err != nil {
			c.recordEventWarningf(cd, "%v", err)
			return
		}

		c.recorder.SetWeight(cd, primaryWeight, canaryWeight)
		c.recordEventWarningf(cd, "Canary failed! Scaling down %s.%s",
			cd.Name, cd.Namespace)

		// shutdown canary
		if err := c.deployer.Scale(cd, 0); err != nil {
			c.recordEventWarningf(cd, "%v", err)
			return
		}

		// mark canary as failed
		if err := c.deployer.SyncStatus(cd, flaggerv1.CanaryStatus{Phase: flaggerv1.CanaryFailed, CanaryWeight: 0}); err != nil {
			c.logger.With("canary", fmt.Sprintf("%s.%s", cd.Name, cd.Namespace)).Errorf("%v", err)
			return
		}

		c.recorder.SetStatus(cd)
		return
	}

	// check if the canary success rate is above the threshold
	// skip check if no traffic is routed to canary
	if canaryWeight == 0 {
		c.recordEventInfof(cd, "Starting canary analysis for %s.%s", cd.Spec.TargetRef.Name, cd.Namespace)
	} else {
		if ok := c.analyseCanary(cd); !ok {
			if err := c.deployer.SetStatusFailedChecks(cd, cd.Status.FailedChecks+1); err != nil {
				c.recordEventWarningf(cd, "%v", err)
				return
			}
			return
		}
	}

	// canary fix routing: A/B testing
	if len(cd.Spec.CanaryAnalysis.Match) > 0 {
		// route traffic to canary and increment iterations
		if cd.Spec.CanaryAnalysis.Iterations > cd.Status.Iterations {
			if err := meshRouter.SetRoutes(cd, 0, 100); err != nil {
				c.recordEventWarningf(cd, "%v", err)
				return
			}
			c.recorder.SetWeight(cd, 0, 100)

			if err := c.deployer.SetStatusIterations(cd, cd.Status.Iterations+1); err != nil {
				c.recordEventWarningf(cd, "%v", err)
				return
			}
			c.recordEventInfof(cd, "Advance %s.%s canary iteration %v/%v",
				cd.Name, cd.Namespace, cd.Status.Iterations+1, cd.Spec.CanaryAnalysis.Iterations)
			return
		}

		// promote canary - max iterations reached
		if cd.Spec.CanaryAnalysis.Iterations == cd.Status.Iterations {
			c.recordEventInfof(cd, "Copying %s.%s template spec to %s.%s",
				cd.Spec.TargetRef.Name, cd.Namespace, primaryName, cd.Namespace)
			if err := c.deployer.Promote(cd); err != nil {
				c.recordEventWarningf(cd, "%v", err)
				return
			}
			// increment iterations
			if err := c.deployer.SetStatusIterations(cd, cd.Status.Iterations+1); err != nil {
				c.recordEventWarningf(cd, "%v", err)
				return
			}
			return
		}

		// shutdown canary
		if cd.Spec.CanaryAnalysis.Iterations < cd.Status.Iterations {
			// route all traffic to the primary
			if err := meshRouter.SetRoutes(cd, 100, 0); err != nil {
				c.recordEventWarningf(cd, "%v", err)
				return
			}
			c.recorder.SetWeight(cd, 100, 0)
			c.recordEventInfof(cd, "Promotion completed! Scaling down %s.%s", cd.Spec.TargetRef.Name, cd.Namespace)

			// canary scale to zero
			if err := c.deployer.Scale(cd, 0); err != nil {
				c.recordEventWarningf(cd, "%v", err)
				return
			}

			// update status phase
			if err := c.deployer.SetStatusPhase(cd, flaggerv1.CanarySucceeded); err != nil {
				c.recordEventWarningf(cd, "%v", err)
				return
			}
			c.recorder.SetStatus(cd)
			c.sendNotification(cd, "Canary analysis completed successfully, promotion finished.",
				false, false)
			return
		}

		return
	}

	// canary incremental traffic weight
	if canaryWeight < maxWeight {
		primaryWeight -= cd.Spec.CanaryAnalysis.StepWeight
		if primaryWeight < 0 {
			primaryWeight = 0
		}
		canaryWeight += cd.Spec.CanaryAnalysis.StepWeight
		if primaryWeight > 100 {
			primaryWeight = 100
		}

		if err := meshRouter.SetRoutes(cd, primaryWeight, canaryWeight); err != nil {
			c.recordEventWarningf(cd, "%v", err)
			return
		}

		// update weight status
		if err := c.deployer.SetStatusWeight(cd, canaryWeight); err != nil {
			c.recordEventWarningf(cd, "%v", err)
			return
		}

		c.recorder.SetWeight(cd, primaryWeight, canaryWeight)
		c.recordEventInfof(cd, "Advance %s.%s canary weight %v", cd.Name, cd.Namespace, canaryWeight)

		// promote canary
		if canaryWeight == maxWeight {
			c.recordEventInfof(cd, "Copying %s.%s template spec to %s.%s",
				cd.Spec.TargetRef.Name, cd.Namespace, primaryName, cd.Namespace)
			if err := c.deployer.Promote(cd); err != nil {
				c.recordEventWarningf(cd, "%v", err)
				return
			}
		}
	} else {
		// route all traffic back to primary
		primaryWeight = 100
		canaryWeight = 0
		if err := meshRouter.SetRoutes(cd, primaryWeight, canaryWeight); err != nil {
			c.recordEventWarningf(cd, "%v", err)
			return
		}

		c.recorder.SetWeight(cd, primaryWeight, canaryWeight)
		c.recordEventInfof(cd, "Promotion completed! Scaling down %s.%s", cd.Spec.TargetRef.Name, cd.Namespace)

		// shutdown canary
		if err := c.deployer.Scale(cd, 0); err != nil {
			c.recordEventWarningf(cd, "%v", err)
			return
		}

		// update status phase
		if err := c.deployer.SetStatusPhase(cd, flaggerv1.CanarySucceeded); err != nil {
			c.recordEventWarningf(cd, "%v", err)
			return
		}
		c.recorder.SetStatus(cd)
		c.sendNotification(cd, "Canary analysis completed successfully, promotion finished.",
			false, false)
	}
}

func (c *Controller) shouldSkipAnalysis(cd *flaggerv1.Canary, meshRouter router.Interface, primaryWeight int, canaryWeight int) bool {
	if !cd.Spec.SkipAnalysis {
		return false
	}

	// route all traffic to primary
	primaryWeight = 100
	canaryWeight = 0
	if err := meshRouter.SetRoutes(cd, primaryWeight, canaryWeight); err != nil {
		c.recordEventWarningf(cd, "%v", err)
		return false
	}
	c.recorder.SetWeight(cd, primaryWeight, canaryWeight)

	// copy spec and configs from canary to primary
	c.recordEventInfof(cd, "Copying %s.%s template spec to %s-primary.%s",
		cd.Spec.TargetRef.Name, cd.Namespace, cd.Spec.TargetRef.Name, cd.Namespace)
	if err := c.deployer.Promote(cd); err != nil {
		c.recordEventWarningf(cd, "%v", err)
		return false
	}

	// shutdown canary
	if err := c.deployer.Scale(cd, 0); err != nil {
		c.recordEventWarningf(cd, "%v", err)
		return false
	}

	// update status phase
	if err := c.deployer.SetStatusPhase(cd, flaggerv1.CanarySucceeded); err != nil {
		c.recordEventWarningf(cd, "%v", err)
		return false
	}

	// notify
	c.recorder.SetStatus(cd)
	c.recordEventInfof(cd, "Promotion completed! Canary analysis was skipped for %s.%s",
		cd.Spec.TargetRef.Name, cd.Namespace)
	c.sendNotification(cd, "Canary analysis was skipped, promotion finished.",
		false, false)

	return true
}

func (c *Controller) checkCanaryStatus(cd *flaggerv1.Canary, shouldAdvance bool) bool {
	c.recorder.SetStatus(cd)
	if cd.Status.Phase == flaggerv1.CanaryProgressing {
		return true
	}

	if cd.Status.Phase == "" {
		if err := c.deployer.SyncStatus(cd, flaggerv1.CanaryStatus{Phase: flaggerv1.CanaryInitialized}); err != nil {
			c.logger.With("canary", fmt.Sprintf("%s.%s", cd.Name, cd.Namespace)).Errorf("%v", err)
			return false
		}
		c.recorder.SetStatus(cd)
		c.recordEventInfof(cd, "Initialization done! %s.%s", cd.Name, cd.Namespace)
		c.sendNotification(cd, "New deployment detected, initialization completed.",
			true, false)
		return false
	}

	if shouldAdvance {
		c.recordEventInfof(cd, "New revision detected! Scaling up %s.%s", cd.Spec.TargetRef.Name, cd.Namespace)
		c.sendNotification(cd, "New revision detected, starting canary analysis.",
			true, false)
		if err := c.deployer.Scale(cd, 1); err != nil {
			c.recordEventErrorf(cd, "%v", err)
			return false
		}
		if err := c.deployer.SyncStatus(cd, flaggerv1.CanaryStatus{Phase: flaggerv1.CanaryProgressing}); err != nil {
			c.logger.With("canary", fmt.Sprintf("%s.%s", cd.Name, cd.Namespace)).Errorf("%v", err)
			return false
		}
		c.recorder.SetStatus(cd)
		return false
	}
	return false
}

func (c *Controller) hasCanaryRevisionChanged(cd *flaggerv1.Canary) bool {
	if cd.Status.Phase == flaggerv1.CanaryProgressing {
		if diff, _ := c.deployer.IsNewSpec(cd); diff {
			return true
		}
		if diff, _ := c.deployer.configTracker.HasConfigChanged(cd); diff {
			return true
		}
	}
	return false
}

func (c *Controller) analyseCanary(r *flaggerv1.Canary) bool {
	// run external checks
	for _, webhook := range r.Spec.CanaryAnalysis.Webhooks {
		err := CallWebhook(r.Name, r.Namespace, webhook)
		if err != nil {
			c.recordEventWarningf(r, "Halt %s.%s advancement external check %s failed %v",
				r.Name, r.Namespace, webhook.Name, err)
			return false
		}
	}

	// run metrics checks
	for _, metric := range r.Spec.CanaryAnalysis.Metrics {
		if metric.Interval == "" {
			metric.Interval = r.GetMetricInterval()
		}

		if metric.Name == "envoy_cluster_upstream_rq" {
			val, err := c.observer.GetEnvoySuccessRate(r.Spec.TargetRef.Name, r.Namespace, metric.Name, metric.Interval)
			if err != nil {
				if strings.Contains(err.Error(), "no values found") {
					c.recordEventWarningf(r, "Halt advancement no values found for metric %s probably %s.%s is not receiving traffic",
						metric.Name, r.Spec.TargetRef.Name, r.Namespace)
				} else {
					c.recordEventErrorf(r, "Metrics server %s query failed: %v", c.observer.metricsServer, err)
				}
				return false
			}
			if float64(metric.Threshold) > val {
				c.recordEventWarningf(r, "Halt %s.%s advancement success rate %.2f%% < %v%%",
					r.Name, r.Namespace, val, metric.Threshold)
				return false
			}
		}

		if metric.Name == "istio_requests_total" {
			val, err := c.observer.GetDeploymentCounter(r.Spec.TargetRef.Name, r.Namespace, metric.Name, metric.Interval)
			if err != nil {
				if strings.Contains(err.Error(), "no values found") {
					c.recordEventWarningf(r, "Halt advancement no values found for metric %s probably %s.%s is not receiving traffic",
						metric.Name, r.Spec.TargetRef.Name, r.Namespace)
				} else {
					c.recordEventErrorf(r, "Metrics server %s query failed: %v", c.observer.metricsServer, err)
				}
				return false
			}
			if float64(metric.Threshold) > val {
				c.recordEventWarningf(r, "Halt %s.%s advancement success rate %.2f%% < %v%%",
					r.Name, r.Namespace, val, metric.Threshold)
				return false
			}
		}

		if metric.Name == "istio_request_duration_seconds_bucket" {
			val, err := c.observer.GetDeploymentHistogram(r.Spec.TargetRef.Name, r.Namespace, metric.Name, metric.Interval)
			if err != nil {
				c.recordEventErrorf(r, "Metrics server %s query failed: %v", c.observer.metricsServer, err)
				return false
			}
			t := time.Duration(metric.Threshold) * time.Millisecond
			if val > t {
				c.recordEventWarningf(r, "Halt %s.%s advancement request duration %v > %v",
					r.Name, r.Namespace, val, t)
				return false
			}
		}

		if metric.Query != "" {
			val, err := c.observer.GetScalar(metric.Query)
			if err != nil {
				if strings.Contains(err.Error(), "no values found") {
					c.recordEventWarningf(r, "Halt advancement no values found for metric %s probably %s.%s is not receiving traffic",
						metric.Name, r.Spec.TargetRef.Name, r.Namespace)
				} else {
					c.recordEventErrorf(r, "Metrics server %s query failed: %v", c.observer.metricsServer, err)
				}
				return false
			}
			if val > float64(metric.Threshold) {
				c.recordEventWarningf(r, "Halt %s.%s advancement %s %.2f > %v",
					r.Name, r.Namespace, metric.Name, val, metric.Threshold)
				return false
			}
		}
	}

	return true
}
