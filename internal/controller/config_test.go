package controller

import (
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testConfig() Config {
	return Config{Cluster: "test-cluster", StateNamespace: "monitoring", StateName: "healthchecks", DefaultInclude: true, Grace: 300, Timezone: "UTC", Channels: "*", Resync: 5 * time.Minute}
}
func testCronJob() *batchv1.CronJob {
	return &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "backup", Namespace: "jobs", UID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}, Spec: batchv1.CronJobSpec{Schedule: "*/5 * * * *"}}
}

func TestIdentitySurvivesRecreationAndSeparatesClusters(t *testing.T) {
	config := testConfig()
	job := testCronJob()
	first, _ := config.Spec(job)
	job.UID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	second, _ := config.Spec(job)
	if first.Slug != second.Slug {
		t.Fatal("identity changed on recreation")
	}
	config.Cluster = "other"
	third, _ := config.Spec(job)
	if first.Slug == third.Slug {
		t.Fatal("clusters share identity")
	}
	config = testConfig()
	job.Namespace = "other"
	fourth, _ := config.Spec(job)
	if first.Slug == fourth.Slug {
		t.Fatal("namespaces share identity")
	}
}

func TestAnnotationConfiguration(t *testing.T) {
	config := testConfig()
	job := testCronJob()
	tz := "Europe/Berlin"
	job.Spec.TimeZone = &tz
	job.Spec.Schedule = "@daily"
	job.Annotations = map[string]string{Prefix + "name": "Nightly backup", Prefix + "grace-seconds": "900", Prefix + "tags": "backup,critical daily", Prefix + "channels": "on-call", Prefix + "description": "Runbook"}
	spec, err := config.Spec(job)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "Nightly backup" || spec.Grace != 900 || spec.Channels != "on-call" || spec.Timezone != tz || spec.Schedule != "0 0 * * *" || !spec.ManualResume {
		t.Fatalf("incorrect settings: %+v", spec)
	}
	if !hasTag(spec.Tags, config.OwnerTag()) || !hasTag(spec.Tags, "critical") {
		t.Fatal("missing tags")
	}
	job.Annotations[Prefix+"channels"] = ""
	spec, _ = config.Spec(job)
	if spec.Channels != "" {
		t.Fatal("explicitly empty channels ignored")
	}
	for _, value := range []string{"0", "59", "31536001", "oops"} {
		job.Annotations[Prefix+"grace-seconds"] = value
		if _, err := config.Spec(job); err == nil {
			t.Fatalf("accepted grace %q", value)
		}
	}
}

func TestInclusion(t *testing.T) {
	for _, tc := range []struct {
		defaultInclude bool
		annotations    map[string]string
		want           bool
		bad            bool
	}{
		{true, nil, true, false}, {false, nil, false, false},
		{false, map[string]string{Prefix + "include": "true"}, true, false},
		{true, map[string]string{Prefix + "exclude": "true"}, false, false},
		{false, map[string]string{Prefix + "include": "true", Prefix + "exclude": "true"}, false, false},
		{true, map[string]string{Prefix + "include": "maybe"}, false, true},
	} {
		config := testConfig()
		config.DefaultInclude = tc.defaultInclude
		job := testCronJob()
		job.Annotations = tc.annotations
		got, err := config.Included(job)
		if (err != nil) != tc.bad || got != tc.want {
			t.Fatalf("inclusion got %v, %v for %+v", got, err, tc)
		}
	}
}

func TestLongNamesAndReservedTags(t *testing.T) {
	config := testConfig()
	job := testCronJob()
	job.Name = strings.Repeat("a", 52)
	job.Namespace = strings.Repeat("b", 63)
	spec, err := config.Spec(job)
	if err != nil || len(spec.Name) > 100 {
		t.Fatalf("long Kubernetes identity failed: %v", err)
	}
	job.Annotations = map[string]string{Prefix + "name": strings.Repeat("x", 101)}
	if _, err := config.Spec(job); err == nil {
		t.Fatal("long custom name accepted")
	}
	job.Annotations = map[string]string{Prefix + "tags": "hck-owner-someone-else"}
	if _, err := config.Spec(job); err == nil {
		t.Fatal("reserved ownership tag accepted")
	}
}

func TestConfigValidation(t *testing.T) {
	config := testConfig()
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, cluster := range []string{"", "Uppercase", "has space", "-bad"} {
		config.Cluster = cluster
		if err := config.Validate(); err == nil {
			t.Fatalf("accepted cluster %q", cluster)
		}
	}
	config = testConfig()
	config.Timezone = "invalid/timezone"
	if err := config.Validate(); err == nil {
		t.Fatal("invalid timezone accepted")
	}
}
