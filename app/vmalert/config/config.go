package config

import (
	"crypto/md5"
	"fmt"
	"hash/fnv"
	"io/ioutil"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/app/vmalert/notifier"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/auth"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/envtemplate"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/metricsql"
	"gopkg.in/yaml.v2"
)

// Global contains setting that can apply to the entire config file
type Global struct {
	Tenant string `yaml:"tenant"`
}

// Group contains list of Rules grouped into
// entity with one name and evaluation interval
type Group struct {
	File        string
	Name        string        `yaml:"name"`
	Interval    time.Duration `yaml:"interval,omitempty"`
	Rules       []Rule        `yaml:"rules"`
	Concurrency int           `yaml:"concurrency"`
	Tenant      string        `yaml:"tenant"`
	// Checksum stores the hash of yaml definition for this group.
	// May be used to detect any changes like rules re-ordering etc.
	Checksum string

	// Catches all undefined fields and must be empty after parsing.
	XXX map[string]interface{} `yaml:",inline"`
}

// UnmarshalYAML implements the yaml.Unmarshaler interface.
func (g *Group) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type group Group
	if err := unmarshal((*group)(g)); err != nil {
		return err
	}
	b, err := yaml.Marshal(g)
	if err != nil {
		return fmt.Errorf("failed to marshal group configuration for checksum: %w", err)
	}
	h := md5.New()
	h.Write(b)
	g.Checksum = fmt.Sprintf("%x", h.Sum(nil))
	return nil
}

// Validate check for internal Group or Rule configuration errors
func (g *Group) Validate(validateAnnotations, validateExpressions bool) error {
	if g.Name == "" {
		return fmt.Errorf("group name must be set")
	}
	if len(g.Rules) == 0 {
		return fmt.Errorf("group %q can't contain no rules", g.Name)
	}
	// validate tenancy setting
	if _, err := auth.NewToken(g.Tenant); err != nil {
		return err
	}

	uniqueRules := map[uint64]struct{}{}
	for _, r := range g.Rules {
		ruleName := r.Record
		if r.Alert != "" {
			ruleName = r.Alert
		}
		if _, ok := uniqueRules[r.ID]; ok {
			return fmt.Errorf("rule %q duplicate", ruleName)
		}
		uniqueRules[r.ID] = struct{}{}
		if err := r.Validate(); err != nil {
			return fmt.Errorf("invalid rule %q.%q: %w", g.Name, ruleName, err)
		}
		if validateExpressions {
			if _, err := metricsql.Parse(r.Expr); err != nil {
				return fmt.Errorf("invalid expression for rule %q.%q: %w", g.Name, ruleName, err)
			}
		}
		if validateAnnotations {
			if err := notifier.ValidateTemplates(r.Annotations); err != nil {
				return fmt.Errorf("invalid annotations for rule %q.%q: %w", g.Name, ruleName, err)
			}
			if err := notifier.ValidateTemplates(r.Labels); err != nil {
				return fmt.Errorf("invalid labels for rule %q.%q: %w", g.Name, ruleName, err)
			}
		}
	}
	return checkOverflow(g.XXX, fmt.Sprintf("group %q", g.Name))
}

// Rule describes entity that represent either
// recording rule or alerting rule.
type Rule struct {
	ID          uint64
	Record      string            `yaml:"record,omitempty"`
	Alert       string            `yaml:"alert,omitempty"`
	Expr        string            `yaml:"expr"`
	For         time.Duration     `yaml:"for,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`

	// Catches all undefined fields and must be empty after parsing.
	XXX map[string]interface{} `yaml:",inline"`
}

// UnmarshalYAML implements the yaml.Unmarshaler interface.
func (r *Rule) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rule Rule
	if err := unmarshal((*rule)(r)); err != nil {
		return err
	}
	r.ID = HashRule(*r)
	return nil
}

// Name returns Rule name according to its type
func (r *Rule) Name() string {
	if r.Record != "" {
		return r.Record
	}
	return r.Alert
}

// HashRule hashes significant Rule fields into
// unique hash that supposed to define Rule uniqueness
func HashRule(r Rule) uint64 {
	h := fnv.New64a()
	h.Write([]byte(r.Expr))
	if r.Record != "" {
		h.Write([]byte("recording"))
		h.Write([]byte(r.Record))
	} else {
		h.Write([]byte("alerting"))
		h.Write([]byte(r.Alert))
	}
	kv := sortMap(r.Labels)
	for _, i := range kv {
		h.Write([]byte(i.key))
		h.Write([]byte(i.value))
		h.Write([]byte("\xff"))
	}
	return h.Sum64()
}

// Validate check for Rule configuration errors
func (r *Rule) Validate() error {
	if (r.Record == "" && r.Alert == "") || (r.Record != "" && r.Alert != "") {
		return fmt.Errorf("either `record` or `alert` must be set")
	}
	if r.Expr == "" {
		return fmt.Errorf("expression can't be empty")
	}
	return checkOverflow(r.XXX, "rule")
}

// Parse parses rule configs from given file patterns
func Parse(pathPatterns []string, validateAnnotations, validateExpressions bool) ([]Group, error) {
	var fp []string
	for _, pattern := range pathPatterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("error reading file pattern %s: %w", pattern, err)
		}
		fp = append(fp, matches...)
	}
	var groups []Group
	for _, file := range fp {
		uniqueGroups := map[string]struct{}{}
		gr, err := parseFile(file)
		if err != nil {
			return nil, fmt.Errorf("failed to parse file %q: %w", file, err)
		}
		for _, g := range gr {
			if err := g.Validate(validateAnnotations, validateExpressions); err != nil {
				return nil, fmt.Errorf("invalid group %q in file %q: %w", g.Name, file, err)
			}
			if _, ok := uniqueGroups[g.Name]; ok {
				return nil, fmt.Errorf("group name %q duplicate in file %q", g.Name, file)
			}
			uniqueGroups[g.Name] = struct{}{}
			g.File = file
			groups = append(groups, g)
		}
	}
	if len(groups) < 1 {
		logger.Warnf("no groups found in %s", strings.Join(pathPatterns, ";"))
	}
	return groups, nil
}

func parseFile(path string) ([]Group, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("error reading alert rule file: %w", err)
	}
	data = envtemplate.Replace(data)
	g := struct {
		Global *Global `yaml:"global"`
		Groups []Group `yaml:"groups"`
		// Catches all undefined fields and must be empty after parsing.
		XXX map[string]interface{} `yaml:",inline"`
	}{}
	err = yaml.Unmarshal(data, &g)
	if err != nil {
		return nil, err
	}
	g.Groups = applyGlobal(g.Groups, g.Global)
	return g.Groups, checkOverflow(g.XXX, "config")
}

// applyGlobal apply all the global settings into groups
// the global setting will get overwritten by group setting
func applyGlobal(groups []Group, global *Global) []Group {
	// fast path: no global settings
	if global == nil {
		return groups
	}

	for _, g := range groups {
		if g.Tenant == "" {
			g.Tenant = global.Tenant
		}
	}

	return groups
}

func checkOverflow(m map[string]interface{}, ctx string) error {
	if len(m) > 0 {
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		return fmt.Errorf("unknown fields in %s: %s", ctx, strings.Join(keys, ", "))
	}
	return nil
}

type item struct {
	key, value string
}

func sortMap(m map[string]string) []item {
	var kv []item
	for k, v := range m {
		kv = append(kv, item{key: k, value: v})
	}
	sort.Slice(kv, func(i, j int) bool {
		return kv[i].key < kv[j].key
	})
	return kv
}
