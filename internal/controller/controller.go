package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Placetel/healthchecks-kubernetes/internal/healthchecks"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	batchlisters "k8s.io/client-go/listers/batch/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

type Backend interface {
	List(context.Context, string) ([]healthchecks.Check, error)
	Upsert(context.Context, healthchecks.Spec) (healthchecks.Check, error)
	Pause(context.Context, string) error
	Resume(context.Context, string) error
	Ping(context.Context, healthchecks.Check, string, string) error
}

type cachedCheck struct {
	spec   healthchecks.Spec
	check  healthchecks.Check
	synced time.Time
}

type Controller struct {
	config   Config
	kube     kubernetes.Interface
	backend  Backend
	factory  informers.SharedInformerFactory
	cronjobs batchlisters.CronJobLister
	jobs     cache.Indexer
	synced   []cache.InformerSynced
	queue    workqueue.TypedRateLimitingInterface[string]
	checks   map[string]cachedCheck
	cutoff   time.Time
	ready    atomic.Bool
}

const ownerIndex = "cronjob-owner"
const sweepKey = "/"

func New(config Config, kube kubernetes.Interface, backend Backend) (*Controller, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	factory := informers.NewSharedInformerFactoryWithOptions(kube, config.Resync, informers.WithNamespace(config.Namespace))
	cj := factory.Batch().V1().CronJobs()
	jobs := factory.Batch().V1().Jobs().Informer()
	if err := jobs.AddIndexers(cache.Indexers{ownerIndex: func(obj any) ([]string, error) {
		job := obj.(*batchv1.Job)
		owner := metav1.GetControllerOf(job)
		if owner == nil || owner.Kind != "CronJob" || owner.APIVersion != "batch/v1" {
			return nil, nil
		}
		return []string{job.Namespace + "/" + owner.Name}, nil
	}}); err != nil {
		return nil, err
	}
	c := &Controller{config: config, kube: kube, backend: backend, factory: factory, cronjobs: cj.Lister(), jobs: jobs.GetIndexer(),
		synced: []cache.InformerSynced{cj.Informer().HasSynced, jobs.HasSynced},
		queue:  workqueue.NewTypedRateLimitingQueue(workqueue.NewTypedItemExponentialFailureRateLimiter[string](time.Second, time.Minute)), checks: make(map[string]cachedCheck)}
	enqueueCronJob := func(obj any) {
		if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			obj = tombstone.Obj
		}
		if job, ok := obj.(*batchv1.CronJob); ok {
			c.queue.Add(job.Namespace + "/" + job.Name)
		}
	}
	enqueueJob := func(obj any) {
		if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			obj = tombstone.Obj
		}
		if job, ok := obj.(*batchv1.Job); ok {
			owner := metav1.GetControllerOf(job)
			if owner != nil && owner.Kind == "CronJob" && owner.APIVersion == "batch/v1" {
				c.queue.Add(job.Namespace + "/" + owner.Name)
			}
		}
	}
	if _, err := cj.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: enqueueCronJob, UpdateFunc: func(_, obj any) { enqueueCronJob(obj) }, DeleteFunc: enqueueCronJob}); err != nil {
		return nil, err
	}
	if _, err := jobs.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: enqueueJob, UpdateFunc: func(_, obj any) { enqueueJob(obj) }}); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Controller) Ready() bool { return c.ready.Load() }

// Run must only be called by the leader. A single worker preserves per-check
// ordering while client-go's queue supplies backoff and duplicate suppression.
func (c *Controller) Run(ctx context.Context) error {
	cutoff, err := c.loadCutoff(ctx)
	if err != nil {
		return err
	}
	c.cutoff = cutoff
	c.factory.Start(ctx.Done())
	defer c.factory.Shutdown()
	if !cache.WaitForCacheSync(ctx.Done(), c.synced...) {
		return errors.New("Kubernetes cache did not synchronize")
	}
	c.ready.Store(true)
	defer c.ready.Store(false)
	c.queue.Add(sweepKey)
	go func() { <-ctx.Done(); c.queue.ShutDown() }()
	ticker := time.NewTicker(c.config.Resync)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.queue.Add(sweepKey)
			}
		}
	}()
	for {
		key, shutdown := c.queue.Get()
		if shutdown {
			return nil
		}
		err := c.reconcile(ctx, key)
		var deferred *healthchecks.RetryAfterError
		if errors.As(err, &deferred) {
			c.queue.Forget(key)
			c.queue.AddAfter(key, deferred.Delay)
		} else if err != nil {
			// Backend errors deliberately omit response bodies and secret-bearing URLs.
			slog.Error("reconciliation failed; retrying", "resource", key, "error", err)
			c.queue.AddRateLimited(key)
		} else {
			c.queue.Forget(key)
		}
		c.queue.Done(key)
	}
}

func (c *Controller) loadCutoff(ctx context.Context) (time.Time, error) {
	cms := c.kube.CoreV1().ConfigMaps(c.config.StateNamespace)
	cm, err := cms.Get(ctx, c.config.StateName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		cm, err = cms.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: c.config.StateName}, Data: map[string]string{
			"owner": c.config.OwnerTag(), "activatedAt": time.Now().UTC().Format(time.RFC3339Nano),
		}}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			cm, err = cms.Get(ctx, c.config.StateName, metav1.GetOptions{})
		}
	}
	if err != nil {
		return time.Time{}, errors.New("cannot load or create controller state ConfigMap")
	}
	if cm.Data["owner"] != c.config.OwnerTag() {
		return time.Time{}, errors.New("controller state belongs to a different cluster or namespace scope; use a different state name")
	}
	cutoff, err := time.Parse(time.RFC3339Nano, cm.Data["activatedAt"])
	if err != nil {
		return time.Time{}, errors.New("invalid activation time in controller state ConfigMap")
	}
	return cutoff, nil
}

func (c *Controller) reconcile(ctx context.Context, key string) error {
	if key == sweepKey {
		return c.sweep(ctx)
	}
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	cronjob, err := c.cronjobs.CronJobs(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		delete(c.checks, key)
		c.queue.Add(sweepKey)
		return nil
	}
	if err != nil {
		return errors.New("cannot read CronJob cache")
	}
	included, err := c.config.Included(cronjob)
	if err != nil {
		return err
	}
	if !included {
		delete(c.checks, key)
		c.queue.Add(sweepKey)
		return nil
	}
	spec, err := c.config.Spec(cronjob)
	if err != nil {
		return err
	}
	entry, ok := c.checks[key]
	if !ok || !reflect.DeepEqual(entry.spec, spec) || time.Since(entry.synced) >= c.config.Resync {
		check, err := c.backend.Upsert(ctx, spec)
		if err != nil {
			return err
		}
		entry = cachedCheck{spec: spec, check: check, synced: time.Now()}
		c.checks[key] = entry
	}
	suspended := cronjob.Spec.Suspend != nil && *cronjob.Spec.Suspend
	if suspended {
		if entry.check.Status != "paused" {
			if err := c.backend.Pause(ctx, entry.check.UUID); err != nil {
				return err
			}
			entry.check.Status = "paused"
			c.checks[key] = entry
		}
		return nil
	}
	if entry.check.Status == "paused" {
		if err := c.backend.Resume(ctx, entry.check.UUID); err != nil {
			return err
		}
		entry.check.Status = "new"
		c.checks[key] = entry
	}
	if entry.check.Status == "new" {
		// Healthchecks does not arm a new (or resumed) check until its first
		// ping. Explicitly label this baseline; it is not a Job execution.
		if err := c.backend.Ping(ctx, entry.check, string(cronjob.UID), "initialize"); err != nil {
			return err
		}
		entry.check.Status = "up"
		c.checks[key] = entry
	}
	objects, err := c.jobs.ByIndex(ownerIndex, key)
	if err != nil {
		return errors.New("cannot read Job cache")
	}
	jobs := make([]*batchv1.Job, 0, len(objects))
	for _, obj := range objects {
		job := obj.(*batchv1.Job)
		owner := metav1.GetControllerOf(job)
		if owner != nil && owner.UID == cronjob.UID {
			jobs = append(jobs, job)
		}
	}
	// Replay retained completions oldest first, then currently running Jobs.
	sort.Slice(jobs, func(i, j int) bool {
		ti, tj := signalTime(jobs[i]), signalTime(jobs[j])
		if ti.Equal(tj) {
			return jobs[i].Name < jobs[j].Name
		}
		return ti.Before(tj)
	})
	for _, job := range jobs {
		if err := c.deliver(ctx, entry.check, job); err != nil {
			return err
		}
	}
	return nil
}

// sweep only pauses checks carrying this controller's exact ownership tag.
// Invalid annotations preserve existing checks until the configuration is fixed.
func (c *Controller) sweep(ctx context.Context) error {
	cronjobs, err := c.cronjobs.List(labels.Everything())
	if err != nil {
		return errors.New("cannot list CronJob cache")
	}
	desired := make(map[string]bool, len(cronjobs))
	for _, job := range cronjobs {
		included, err := c.config.Included(job)
		if included || err != nil {
			desired[c.config.Slug(job.Namespace, job.Name)] = true
		}
		if included {
			c.queue.Add(job.Namespace + "/" + job.Name)
		}
	}
	checks, err := c.backend.List(ctx, c.config.OwnerTag())
	if err != nil {
		return err
	}
	for _, check := range checks {
		if !hasTag(check.Tags, c.config.OwnerTag()) || desired[check.Slug] || check.Status == "paused" {
			continue
		}
		if err := c.backend.Pause(ctx, check.UUID); err != nil {
			return err
		}
	}
	return nil
}

func hasTag(tags, tag string) bool {
	for _, value := range strings.Fields(tags) {
		if value == tag {
			return true
		}
	}
	return false
}

type delivery struct {
	Check  string `json:"check"`
	Signal string `json:"signal"`
}

func terminal(job *batchv1.Job) (string, time.Time) {
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		if condition.Type == batchv1.JobFailed {
			return "fail", condition.LastTransitionTime.Time
		}
		if condition.Type == batchv1.JobComplete {
			return "success", condition.LastTransitionTime.Time
		}
	}
	return "", time.Time{}
}

func signalTime(job *batchv1.Job) time.Time {
	_, at := terminal(job)
	if !at.IsZero() {
		return at
	}
	// Place active Jobs after retained completions: never leave a stale success
	// after reporting the start of a currently running Job.
	return time.Date(9999, time.December, 31, 0, 0, 0, 0, time.UTC)
}

func (c *Controller) deliver(ctx context.Context, check healthchecks.Check, cached *batchv1.Job) error {
	// A direct read sees delivery checkpoints even when the informer is behind.
	job, err := c.kube.BatchV1().Jobs(cached.Namespace).Get(ctx, cached.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return errors.New("cannot read Job delivery state")
	}
	if job.UID != cached.UID {
		return nil
	}
	signal, at := terminal(job)
	if signal != "" {
		if at.IsZero() {
			return errors.New("terminal Job has no transition timestamp")
		}
		if at.Before(c.cutoff) {
			return nil
		}
	} else {
		if job.Status.StartTime == nil || job.Spec.Suspend != nil && *job.Spec.Suspend {
			return nil
		}
		signal = "start"
	}
	var previous delivery
	if raw := job.Annotations[DeliveryAnnotation]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &previous); err != nil {
			return errors.New("invalid Job delivery annotation")
		}
	}
	fingerprint := sha256.Sum256([]byte(check.UUID))
	checkIdentity := hex.EncodeToString(fingerprint[:])
	if previous.Check == checkIdentity && (previous.Signal == signal || previous.Signal == "success" || previous.Signal == "fail") {
		return nil
	}
	if err := c.backend.Ping(ctx, check, string(job.UID), signal); err != nil {
		return err
	}
	marker, _ := json.Marshal(delivery{Check: checkIdentity, Signal: signal})
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{
		"uid": job.UID, "resourceVersion": job.ResourceVersion, "annotations": map[string]string{DeliveryAnnotation: string(marker)},
	}})
	if _, err := c.kube.BatchV1().Jobs(job.Namespace).Patch(ctx, job.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("ping accepted but Job checkpoint failed; retry may repeat ping")
	}
	return nil
}
