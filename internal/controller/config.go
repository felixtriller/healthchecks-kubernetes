package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/felixtriller/healthchecks-kubernetes/internal/healthchecks"
	batchv1 "k8s.io/api/batch/v1"
)

const Prefix = "healthchecks-kubernetes.felixtriller.github.io/"
const DeliveryAnnotation = Prefix + "delivery"

var clusterPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type Config struct {
	Cluster        string
	Namespace      string
	StateNamespace string
	StateName      string
	DefaultInclude bool
	Grace          int
	Timezone       string
	Channels       string
	Resync         time.Duration
}

func (c Config) Validate() error {
	if !clusterPattern.MatchString(c.Cluster) {
		return errors.New("cluster must be 1-63 lowercase letters, digits, or hyphens, starting with a letter or digit")
	}
	if c.StateNamespace == "" || c.StateName == "" {
		return errors.New("state namespace and name are required")
	}
	if c.Grace < 60 || c.Grace > 31536000 {
		return errors.New("grace must be between 60 and 31536000 seconds")
	}
	if c.Resync < time.Second {
		return errors.New("resync must be at least one second")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return errors.New("invalid default timezone")
	}
	return nil
}

// Ownership includes the watch scope, so namespace-scoped controllers never
// retire one another's checks. Do not run overlapping scopes for a cluster.
func (c Config) OwnerTag() string {
	hash := sha256.Sum256([]byte(c.Cluster + "\x00" + c.Namespace))
	return "hck-owner-" + hex.EncodeToString(hash[:12])
}

func (c Config) Slug(namespace, name string) string {
	hash := sha256.Sum256([]byte(c.Cluster + "\x00" + namespace + "\x00" + name))
	return "k8s-" + hex.EncodeToString(hash[:20])
}

func (c Config) Included(job *batchv1.CronJob) (bool, error) {
	included := c.DefaultInclude
	for _, setting := range []string{"include", "exclude"} {
		value, exists := job.Annotations[Prefix+setting]
		if !exists {
			continue
		}
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return false, fmt.Errorf("%s annotation must be a boolean", setting)
		}
		if setting == "include" {
			included = parsed
		} else if parsed {
			included = false
		}
	}
	return included, nil
}

func (c Config) Spec(job *batchv1.CronJob) (healthchecks.Spec, error) {
	grace := c.Grace
	if value, ok := job.Annotations[Prefix+"grace-seconds"]; ok {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 60 || parsed > 31536000 {
			return healthchecks.Spec{}, errors.New("grace-seconds must be between 60 and 31536000")
		}
		grace = parsed
	}
	tz := c.Timezone
	if job.Spec.TimeZone != nil {
		tz = *job.Spec.TimeZone
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return healthchecks.Spec{}, errors.New("CronJob has invalid timezone")
	}
	name := c.Cluster + "/" + job.Namespace + "/" + job.Name
	if value := job.Annotations[Prefix+"name"]; value != "" {
		name = value
		if utf8.RuneCountInString(name) > 100 {
			return healthchecks.Spec{}, errors.New("name annotation must be at most 100 characters")
		}
	} else if utf8.RuneCountInString(name) > 100 {
		hash := sha256.Sum256([]byte(name))
		name = string([]rune(name)[:87]) + "-" + hex.EncodeToString(hash[:6])
	}
	channels := c.Channels
	if value, ok := job.Annotations[Prefix+"channels"]; ok {
		channels = value
	}
	tags := []string{"healthchecks-kubernetes", c.OwnerTag(), "cluster:" + c.Cluster, "namespace:" + job.Namespace}
	for _, tag := range strings.Fields(strings.ReplaceAll(job.Annotations[Prefix+"tags"], ",", " ")) {
		if strings.HasPrefix(tag, "hck-owner-") {
			return healthchecks.Spec{}, errors.New("hck-owner- tags are reserved")
		}
		tags = append(tags, tag)
	}
	return healthchecks.Spec{
		Name: name, Slug: c.Slug(job.Namespace, job.Name), Tags: strings.Join(tags, " "),
		Description: job.Annotations[Prefix+"description"], Schedule: normalizeSchedule(job.Spec.Schedule),
		Timezone: tz, Grace: grace, Channels: channels, ManualResume: true,
	}, nil
}

// Kubernetes accepts these cron macros; Healthchecks expects five fields.
func normalizeSchedule(schedule string) string {
	switch schedule {
	case "@yearly", "@annually":
		return "0 0 1 1 *"
	case "@monthly":
		return "0 0 1 * *"
	case "@weekly":
		return "0 0 * * 0"
	case "@daily", "@midnight":
		return "0 0 * * *"
	case "@hourly":
		return "0 * * * *"
	default:
		return schedule
	}
}
