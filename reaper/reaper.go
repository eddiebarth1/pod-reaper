package main

import (
	"context"
	"errors"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
)

type reaper struct {
	clientSet kubernetes.Interface
	options   options
}

func newReaper() reaper {
	config, err := rest.InClusterConfig()
	if err != nil {
		logrus.WithError(err).Panic("error getting in cluster kubernetes config")
	}
	clientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		logrus.WithError(err).Panic("unable to get client set for in cluster kubernetes config")
	}
	if clientSet == nil {
		logrus.Panic("kubernetes client set cannot be nil")
	}
	options, err := loadOptions()
	if err != nil {
		logrus.WithError(err).Panic("error loading options")
	}
	return reaper{
		clientSet: clientSet,
		options:   options,
	}
}

// isTransientError returns true for errors that are likely transient and worth retrying
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	// Retry on rate limiting (429)
	if apierrors.IsTooManyRequests(err) {
		return true
	}
	// Retry on server errors (5xx)
	if apierrors.IsServerTimeout(err) || apierrors.IsServiceUnavailable(err) {
		return true
	}
	// Retry on internal errors
	if apierrors.IsInternalError(err) {
		return true
	}
	// Retry on context deadline exceeded (timeout) - may be transient
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return false
}

func (reaper reaper) getPods(ctx context.Context) (*v1.PodList, error) {
	coreClient := reaper.clientSet.CoreV1()
	pods := coreClient.Pods(reaper.options.namespace)
	listOptions := metav1.ListOptions{}
	if reaper.options.labelExclusion != nil || reaper.options.labelRequirement != nil {
		selector := labels.NewSelector()
		if reaper.options.labelExclusion != nil {
			selector = selector.Add(*reaper.options.labelExclusion)
		}
		if reaper.options.labelRequirement != nil {
			selector = selector.Add(*reaper.options.labelRequirement)
		}
		listOptions.LabelSelector = selector.String()
	}

	var podList *v1.PodList
	backoff := retry.DefaultBackoff
	backoff.Steps = 3 // Max 3 retries

	err := retry.OnError(backoff, isTransientError, func() error {
		apiCtx, cancel := context.WithTimeout(ctx, reaper.options.apiTimeout)
		defer cancel()

		var listErr error
		podList, listErr = pods.List(apiCtx, listOptions)
		return listErr
	})

	if err != nil {
		return nil, err
	}

	reaper.options.podSortingStrategy(podList.Items)
	if reaper.options.annotationRequirement != nil {
		podList.Items = filter(reaper, podList.Items...)
	}
	return podList, nil
}

func filter(reaper reaper, pods ...v1.Pod) []v1.Pod {
	var filtered []v1.Pod
	for _, pod := range pods {
		selector := labels.Set(pod.Annotations)
		if reaper.options.annotationRequirement.Matches(selector) {
			filtered = append(filtered, pod)
		}
	}
	return filtered
}

func (reaper reaper) reapPod(ctx context.Context, pod v1.Pod, reasons []string, reapedPods int) {
	deleteOptions := &metav1.DeleteOptions{
		GracePeriodSeconds: reaper.options.gracePeriod,
	}

	podLog := logrus.WithFields(logrus.Fields{
		"pod":       pod.Name,
		"namespace": pod.Namespace,
		"reasons":   reasons,
	})

	if reaper.options.dryRun {
		podLog.Info("pod would be reaped but pod-reaper is in dry-run mode")
		return
	}

	if reaper.options.maxPods > 0 && reapedPods >= reaper.options.maxPods {
		podLog.WithFields(logrus.Fields{
			"reapedPods": reapedPods,
			"maxPods":    reaper.options.maxPods,
		}).Info("pod would be reaped but maxPods is exceeded")
		return
	}

	podLog.Info("reaping pod")

	// Create context with timeout for the API call
	apiCtx, cancel := context.WithTimeout(ctx, reaper.options.apiTimeout)
	defer cancel()

	var err error
	if reaper.options.evict {
		err = reaper.clientSet.PolicyV1().Evictions(pod.Namespace).Evict(apiCtx, &policyv1.Eviction{
			ObjectMeta:    metav1.ObjectMeta{Namespace: pod.Namespace, Name: pod.Name},
			DeleteOptions: deleteOptions,
		})
	} else {
		err = reaper.clientSet.CoreV1().Pods(pod.Namespace).Delete(apiCtx, pod.Name, *deleteOptions)
	}
	if err != nil {
		podLog.WithError(err).Warn("unable to delete pod")
	}
}

func (reaper reaper) scytheCycle(ctx context.Context) {
	logrus.WithField("namespace", reaper.options.namespace).Debug("starting reap cycle")

	pods, err := reaper.getPods(ctx)
	if err != nil {
		logrus.WithError(err).WithField("namespace", reaper.options.namespace).
			Error("failed to get pods, skipping cycle")
		return
	}

	reapedPods := 0
	for i, pod := range pods.Items {
		// Check for shutdown signal
		select {
		case <-ctx.Done():
			logrus.Info("shutdown signal received during reap cycle, stopping")
			return
		default:
		}

		shouldReap, reasons := reaper.options.rules.ShouldReap(pod)
		if shouldReap {
			reaper.reapPod(ctx, pod, reasons, reapedPods)
			reapedPods++

			// Apply deletion delay (rate limiting) if configured
			// Don't delay after the last pod
			if reaper.options.deletionDelay > 0 && i < len(pods.Items)-1 {
				select {
				case <-ctx.Done():
					logrus.Info("shutdown signal received during deletion delay, stopping")
					return
				case <-time.After(reaper.options.deletionDelay):
				}
			}
		}
	}

	logrus.WithField("reapedPods", reapedPods).Debug("completed reap cycle")
}

func cronWithOptionalSeconds() *cron.Cron {
	return cron.New(
		cron.WithParser(
			cron.NewParser(
				// include optional seconds
				cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)))
}

func (reaper reaper) harvest(ctx context.Context) {
	runForever := reaper.options.runDuration == 0
	schedule := cronWithOptionalSeconds()
	_, err := schedule.AddFunc(reaper.options.schedule, func() {
		reaper.scytheCycle(ctx)
	})

	if err != nil {
		logrus.WithError(err).WithField("schedule", reaper.options.schedule).
			Panic("unable to create cron schedule")
	}

	schedule.Start()
	logrus.WithField("schedule", reaper.options.schedule).Info("started reap schedule")

	if runForever {
		<-ctx.Done()
		logrus.Info("shutdown signal received, stopping scheduler")
	} else {
		select {
		case <-ctx.Done():
			logrus.Info("shutdown signal received, stopping scheduler")
		case <-time.After(reaper.options.runDuration):
			logrus.Info("run duration elapsed, stopping scheduler")
		}
	}

	// Stop the cron scheduler gracefully
	stopCtx := schedule.Stop()
	<-stopCtx.Done()
	logrus.Info("scheduler stopped cleanly")
}
