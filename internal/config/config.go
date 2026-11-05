// Package config loads chuck's two kinds of configuration: the chuck-wide
// config/chuck.yaml, changed only by pull request, and each consumer
// repo's .claude/loop.json, read from that repo's default branch. Both
// decode strictly, and validation reports every problem at once.
//
// The shapes are the "Config" section of
// docs/superpowers/specs/2026-11-03-delivery-loop-design.md.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.yaml.in/yaml/v3"
)

// Stage is one step of the loop that runs a worker.
type Stage string

const (
	StageIntake   Stage = "intake"
	StageBuild    Stage = "build"
	StageReview   Stage = "review"
	StageSweep    Stage = "sweep"
	StageValidate Stage = "validate"
)

// Stages lists every stage in loop order.
var Stages = []Stage{StageIntake, StageBuild, StageReview, StageSweep, StageValidate}

func validStage(s Stage) bool {
	for _, x := range Stages {
		if x == s {
			return true
		}
	}
	return false
}

// Duration is a time.Duration written as a Go duration string ("90m") in
// YAML and JSON.
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("line %d: want a duration string like \"90m\"", n.Line)
	}
	return d.parse(s)
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("want a duration string like \"90m\"")
	}
	return d.parse(s)
}

func (d *Duration) parse(s string) error {
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// problems collects validation errors so every one is reported together.
type problems []error

func (p *problems) add(format string, args ...any) {
	*p = append(*p, fmt.Errorf(format, args...))
}

func (p problems) err() error { return errors.Join(p...) }
