package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/felixtriller/healthchecks-kubernetes/internal/healthchecks"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

const testCheckID = "11111111-1111-4111-8111-111111111111"
const testJobID = "22222222-2222-4222-8222-222222222222"

type fakeBackend struct {
	checks   []healthchecks.Check
	signals  []string
	runIDs   []string
	paused   []string
	resumed  []string
	upserts  int
	failPing bool
	state    string
}

func (b *fakeBackend) List(context.Context, string) ([]healthchecks.Check, error) {
	return b.checks, nil
}
func (b *fakeBackend) Upsert(_ context.Context, spec healthchecks.Spec) (healthchecks.Check, error) {
	b.upserts++
	status := b.state
	if status == "" {
		status = "up"
	}
	return healthchecks.Check{Spec: spec, UUID: testCheckID, PingURL: "https://ping.example.test/" + testCheckID, Status: status}, nil
}
func (b *fakeBackend) Pause(_ context.Context, id string) error {
	b.paused = append(b.paused, id)
	return nil
}
func (b *fakeBackend) Resume(_ context.Context, id string) error {
	b.resumed = append(b.resumed, id)
	return nil
}
func (b *fakeBackend) Ping(_ context.Context, _ healthchecks.Check, id, signal string) error {
	if b.failPing {
		return errors.New("network unavailable")
	}
	b.signals = append(b.signals, signal)
	b.runIDs = append(b.runIDs, id)
	return nil
}

func testJob(cronjob *batchv1.CronJob) *batchv1.Job {
	start := metav1.NewTime(time.Now().Add(-time.Minute))
	controlling := true
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "backup-123", Namespace: cronjob.Namespace, UID: testJobID,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "CronJob", Name: cronjob.Name, UID: cronjob.UID, Controller: &controlling}},
	}, Status: batchv1.JobStatus{StartTime: &start, Active: 1}}
}
func complete(job *batchv1.Job, kind batchv1.JobConditionType, at time.Time) {
	job.Status.Active = 0
	job.Status.Conditions = []batchv1.JobCondition{{Type: kind, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(at)}}
}
func fixture(t *testing.T, backend *fakeBackend, cronjob *batchv1.CronJob, jobs ...*batchv1.Job) (*Controller, *fake.Clientset) {
	t.Helper()
	objects := []runtime.Object{cronjob}
	for _, job := range jobs {
		objects = append(objects, job)
	}
	kube := fake.NewClientset(objects...)
	c, err := New(testConfig(), kube, backend)
	if err != nil {
		t.Fatal(err)
	}
	c.cutoff = time.Now().Add(-time.Hour)
	if err := c.factory.Batch().V1().CronJobs().Informer().GetStore().Add(cronjob); err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if err := c.jobs.Add(job); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(c.queue.ShutDown)
	return c, kube
}
func reconcile(t *testing.T, c *Controller, job *batchv1.CronJob) {
	t.Helper()
	if err := c.reconcile(context.Background(), job.Namespace+"/"+job.Name); err != nil {
		t.Fatal(err)
	}
}

func TestDeliverySurvivesRestartAndInformerLag(t *testing.T) {
	ctx := context.Background()
	cronjob := testCronJob()
	job := testJob(cronjob)
	backend := &fakeBackend{}
	c, kube := fixture(t, backend, cronjob, job)
	reconcile(t, c, cronjob)
	reconcile(t, c, cronjob) // Deliberately leave informer annotations stale.
	if fmt.Sprint(backend.signals) != "[start]" {
		t.Fatalf("duplicate start: %v", backend.signals)
	}
	current, err := kube.BatchV1().Jobs(job.Namespace).Get(ctx, job.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(current.Annotations[DeliveryAnnotation], testCheckID) {
		t.Fatal("check UUID exposed in Job annotation")
	}
	complete(current, batchv1.JobComplete, time.Now())
	if _, err := kube.BatchV1().Jobs(job.Namespace).UpdateStatus(ctx, current, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(testConfig(), kube, backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.queue.ShutDown)
	restarted.cutoff = c.cutoff
	_ = restarted.factory.Batch().V1().CronJobs().Informer().GetStore().Add(cronjob)
	_ = restarted.jobs.Add(job)
	reconcile(t, restarted, cronjob)
	reconcile(t, restarted, cronjob)
	if fmt.Sprint(backend.signals) != "[start success]" {
		t.Fatalf("unexpected delivery after restart: %v", backend.signals)
	}
	if backend.runIDs[0] != testJobID || backend.runIDs[1] != testJobID {
		t.Fatal("run ID changed across signals")
	}
}

func TestFailureUsesTerminalJobCondition(t *testing.T) {
	cronjob := testCronJob()
	job := testJob(cronjob)
	job.Status.Failed = 2 // Failed Pods do not mean the Job is terminal.
	backend := &fakeBackend{}
	c, kube := fixture(t, backend, cronjob, job)
	reconcile(t, c, cronjob)
	current, _ := kube.BatchV1().Jobs(job.Namespace).Get(context.Background(), job.Name, metav1.GetOptions{})
	complete(current, batchv1.JobFailed, time.Now())
	_, _ = kube.BatchV1().Jobs(job.Namespace).UpdateStatus(context.Background(), current, metav1.UpdateOptions{})
	reconcile(t, c, cronjob)
	if fmt.Sprint(backend.signals) != "[start fail]" {
		t.Fatalf("wrong retry handling: %v", backend.signals)
	}
}

func TestRejectedPingIsRetriedWithoutCheckpoint(t *testing.T) {
	cronjob := testCronJob()
	job := testJob(cronjob)
	backend := &fakeBackend{failPing: true}
	c, kube := fixture(t, backend, cronjob, job)
	if err := c.reconcile(context.Background(), "jobs/backup"); err == nil {
		t.Fatal("expected delivery error")
	}
	current, _ := kube.BatchV1().Jobs(job.Namespace).Get(context.Background(), job.Name, metav1.GetOptions{})
	if current.Annotations[DeliveryAnnotation] != "" {
		t.Fatal("failed ping was checkpointed")
	}
	backend.failPing = false
	reconcile(t, c, cronjob)
	if fmt.Sprint(backend.signals) != "[start]" {
		t.Fatalf("retry failed: %v", backend.signals)
	}
}

func TestCheckpointFailureRetriesAtLeastOnce(t *testing.T) {
	cronjob := testCronJob()
	job := testJob(cronjob)
	backend := &fakeBackend{}
	c, kube := fixture(t, backend, cronjob, job)
	failPatch := true
	kube.PrependReactor("patch", "jobs", func(ktesting.Action) (bool, runtime.Object, error) {
		if failPatch {
			return true, nil, errors.New("conflict")
		}
		return false, nil, nil
	})
	if err := c.reconcile(context.Background(), "jobs/backup"); err == nil {
		t.Fatal("checkpoint failure ignored")
	}
	failPatch = false
	reconcile(t, c, cronjob)
	if fmt.Sprint(backend.signals) != "[start start]" {
		t.Fatalf("expected at-least-once retry: %v", backend.signals)
	}
}

func TestFirstInstallSkipsHistoryButRecoversRecentCompletion(t *testing.T) {
	cronjob := testCronJob()
	job := testJob(cronjob)
	backend := &fakeBackend{}
	complete(job, batchv1.JobComplete, time.Now().Add(-2*time.Hour))
	c, kube := fixture(t, backend, cronjob, job)
	reconcile(t, c, cronjob)
	if len(backend.signals) != 0 {
		t.Fatal("historical completion replayed on first install")
	}
	current, _ := kube.BatchV1().Jobs(job.Namespace).Get(context.Background(), job.Name, metav1.GetOptions{})
	complete(current, batchv1.JobComplete, time.Now())
	_, _ = kube.BatchV1().Jobs(job.Namespace).UpdateStatus(context.Background(), current, metav1.UpdateOptions{})
	reconcile(t, c, cronjob)
	if fmt.Sprint(backend.signals) != "[success]" {
		t.Fatalf("recent completion not recovered: %v", backend.signals)
	}
}

func TestSuspendResumeAndNewCheckActivation(t *testing.T) {
	for _, status := range []string{"new", "paused", "up"} {
		t.Run(status, func(t *testing.T) {
			cronjob := testCronJob()
			backend := &fakeBackend{state: status}
			c, _ := fixture(t, backend, cronjob)
			reconcile(t, c, cronjob)
			wantSignals := 0
			if status == "new" || status == "paused" {
				wantSignals = 1
			}
			if len(backend.signals) != wantSignals {
				t.Fatalf("activation signals: %v", backend.signals)
			}
			if wantSignals == 1 && backend.signals[0] != "initialize" {
				t.Fatal("activation was not labeled")
			}
			if status == "paused" && len(backend.resumed) != 1 {
				t.Fatal("paused check not resumed")
			}
			if status == "new" && len(backend.resumed) != 0 {
				t.Fatal("resume endpoint must not be used for new checks")
			}
		})
	}
	cronjob := testCronJob()
	suspend := true
	cronjob.Spec.Suspend = &suspend
	backend := &fakeBackend{}
	c, _ := fixture(t, backend, cronjob, testJob(cronjob))
	reconcile(t, c, cronjob)
	reconcile(t, c, cronjob)
	if len(backend.paused) != 1 || len(backend.signals) != 0 {
		t.Fatal("suspended job generated signals or repeated pause")
	}
}

func TestCleanupOnlyTouchesOwnedMissingChecks(t *testing.T) {
	cronjob := testCronJob()
	config := testConfig()
	backend := &fakeBackend{checks: []healthchecks.Check{
		{Spec: healthchecks.Spec{Slug: config.Slug(cronjob.Namespace, cronjob.Name), Tags: config.OwnerTag()}, UUID: "present", Status: "up"},
		{Spec: healthchecks.Spec{Slug: "deleted", Tags: config.OwnerTag()}, UUID: "deleted", Status: "up"},
		{Spec: healthchecks.Spec{Slug: "other", Tags: "somebody-else"}, UUID: "foreign", Status: "up"},
		{Spec: healthchecks.Spec{Slug: "already-paused", Tags: config.OwnerTag()}, UUID: "already-paused", Status: "paused"},
	}}
	c, _ := fixture(t, backend, cronjob)
	if err := c.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(backend.paused) != "[deleted]" {
		t.Fatalf("unsafe cleanup: %v", backend.paused)
	}
	cronjob.Annotations = map[string]string{Prefix + "include": "bad-value"}
	_ = c.factory.Batch().V1().CronJobs().Informer().GetStore().Update(cronjob)
	backend.paused = nil
	if err := c.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(backend.paused) != "[deleted]" {
		t.Fatal("invalid inclusion setting caused cleanup")
	}
}

func TestRecreatedCronJobDoesNotReplayOldOwnerJobs(t *testing.T) {
	cronjob := testCronJob()
	job := testJob(cronjob)
	cronjob.UID = types.UID("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	backend := &fakeBackend{}
	c, _ := fixture(t, backend, cronjob, job)
	reconcile(t, c, cronjob)
	if len(backend.signals) != 0 {
		t.Fatal("old CronJob's Job replayed under recreated CronJob")
	}
}

func TestStateCutoffPersistsAndValidatesOwnership(t *testing.T) {
	backend := &fakeBackend{}
	c, kube := fixture(t, backend, testCronJob())
	first, err := c.loadCutoff(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.loadCutoff(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !first.Equal(second) {
		t.Fatal("activation cutoff reset")
	}
	state, _ := kube.CoreV1().ConfigMaps(c.config.StateNamespace).Get(context.Background(), c.config.StateName, metav1.GetOptions{})
	state.Data["owner"] = "different-controller"
	_, _ = kube.CoreV1().ConfigMaps(c.config.StateNamespace).Update(context.Background(), state, metav1.UpdateOptions{})
	if _, err := c.loadCutoff(context.Background()); err == nil {
		t.Fatal("foreign state accepted")
	}
}

type notifyingBackend struct {
	fakeBackend
	pings chan string
}

func (b *notifyingBackend) Ping(ctx context.Context, check healthchecks.Check, id, signal string) error {
	err := b.fakeBackend.Ping(ctx, check, id, signal)
	if err == nil {
		b.pings <- signal
	}
	return err
}

func TestRunSynchronizesWatchesAndStops(t *testing.T) {
	cronjob := testCronJob()
	kube := fake.NewClientset(cronjob, testJob(cronjob))
	backend := &notifyingBackend{pings: make(chan string, 10)}
	c, err := New(testConfig(), kube, backend)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	select {
	case signal := <-backend.pings:
		if signal != "start" {
			t.Fatalf("unexpected watched signal: %s", signal)
		}
		if !c.Ready() {
			t.Fatal("controller not ready after successful delivery")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("informer did not deliver running Job")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("controller did not stop after cancellation")
	}
	if c.Ready() {
		t.Fatal("stopped controller is still ready")
	}
}

func TestCheckpointPatchUsesOnlyUIDPrecondition(t *testing.T) {
	cronjob := testCronJob()
	job := testJob(cronjob)
	c, kube := fixture(t, &fakeBackend{}, cronjob, job)
	var patch []byte
	kube.PrependReactor("patch", "jobs", func(action ktesting.Action) (bool, runtime.Object, error) {
		patch = action.(ktesting.PatchAction).GetPatch()
		return false, nil, nil
	})
	reconcile(t, c, cronjob)
	var body struct {
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(patch, &body); err != nil {
		t.Fatalf("checkpoint patch missing or invalid: %v", err)
	}
	if _, found := body.Metadata["resourceVersion"]; found {
		t.Fatal("checkpoint patch carries a resourceVersion precondition; concurrent status updates would force duplicate pings")
	}
	if string(body.Metadata["uid"]) != `"`+testJobID+`"` {
		t.Fatalf("checkpoint patch must be guarded by the Job UID, got %s", body.Metadata["uid"])
	}
}
