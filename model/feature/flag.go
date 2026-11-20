package feature

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"net/url"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

// Flags is a struct for a set of feature flags.
type Flags struct {
	DocID   string
	DocRev  string
	M       map[string]interface{}
	Sources []*Flags
}

// ID is part of the couchdb.Document interface
func (f *Flags) ID() string { return f.DocID }

// Rev is part of the couchdb.Document interface
func (f *Flags) Rev() string { return f.DocRev }

// DocType is part of the couchdb.Document interface
func (f *Flags) DocType() string { return consts.Settings }

// SetID is part of the couchdb.Document interface
func (f *Flags) SetID(id string) { f.DocID = id }

// SetRev is part of the couchdb.Document interface
func (f *Flags) SetRev(rev string) { f.DocRev = rev }

// Clone is part of the couchdb.Document interface
func (f *Flags) Clone() couchdb.Doc {
	clone := Flags{DocID: f.DocID, DocRev: f.DocRev}
	clone.M = make(map[string]interface{})
	for k, v := range f.M {
		clone.M[k] = v
	}
	return &clone
}

// MarshalJSON is used for marshalling to JSON.
func (f *Flags) MarshalJSON() ([]byte, error) {
	return json.Marshal(f.M)
}

// UnmarshalJSON is used to parse JSON.
func (f *Flags) UnmarshalJSON(bytes []byte) error {
	err := json.Unmarshal(bytes, &f.M)
	if err != nil {
		return err
	}
	if id, ok := f.M["_id"].(string); ok {
		f.SetID(id)
		delete(f.M, "_id")
	}
	if rev, ok := f.M["_rev"].(string); ok {
		f.SetRev(rev)
		delete(f.M, "_rev")
	}
	return nil
}

func (f *Flags) GetList(name string) ([]interface{}, error) {
	if f.M[name] == nil {
		return []interface{}{}, nil
	}

	value, ok := f.M[name].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("Flag %s is not a list flag", name)
	}

	list, ok := value["list"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("Flag %s is not a list flag", name)
	}

	return list, nil
}

func (f *Flags) HasListItem(name, item string) (bool, error) {
	list, err := f.GetList(name)
	if err != nil {
		return false, err
	}

	for _, i := range list {
		if i == item {
			return true, nil
		}
	}
	return false, nil
}

// Enabled reports whether the effective flag is a boolean true.
func Enabled(inst *instance.Instance, name string) bool {
	if value, ok := inst.FeatureFlags[name]; ok {
		enabled, _ := value.(bool)
		return enabled
	}
	flags, err := GetFlags(inst)
	if err != nil {
		return false
	}
	enabled, _ := flags.M[name].(bool)
	return enabled
}

// EnabledFresh resolves a flag in source priority order without the Manager
// cache. Do not authorize work from a lower-priority source on lookup failure.
func EnabledFresh(inst *instance.Instance, name string) (bool, error) {
	if value, ok := inst.FeatureFlags[name]; ok {
		enabled, _ := value.(bool)
		return enabled, nil
	}
	if len(inst.FeatureSets) != 0 {
		flags, err := fetchManagerFlags(inst)
		if err != nil {
			return false, err
		}
		if value, ok := flags[name]; ok {
			enabled, _ := value.(bool)
			return enabled, nil
		}
	}
	flags := &Flags{M: make(map[string]interface{})}
	for _, add := range []func(*instance.Instance) error{flags.addConfig, flags.addContext, flags.addDefaults} {
		if err := add(inst); err != nil {
			return false, err
		}
		if value, ok := flags.M[name]; ok {
			enabled, _ := value.(bool)
			return enabled, nil
		}
	}
	return false, nil
}

// GetFlags returns the list of feature flags for the given instance.
func GetFlags(inst *instance.Instance) (*Flags, error) {
	sources := make([]*Flags, 0)
	m := make(map[string]interface{})
	flags := &Flags{
		DocID:   consts.FlagsSettingsID,
		M:       m,
		Sources: sources,
	}
	flags.addInstanceFlags(inst)
	if err := flags.addManager(inst); err != nil {
		inst.Logger().WithNamespace("flags").
			Warnf("Cannot get the flags from the manager: %s", err)
	}
	if err := flags.addConfig(inst); err != nil {
		inst.Logger().WithNamespace("flags").
			Warnf("Cannot get the flags from the config: %s", err)
	}
	if err := flags.addContext(inst); err != nil {
		inst.Logger().WithNamespace("flags").
			Warnf("Cannot get the flags from the context: %s", err)
	}
	if err := flags.addDefaults(inst); err != nil {
		inst.Logger().WithNamespace("flags").
			Warnf("Cannot get the flags from the defaults: %s", err)
	}
	return flags, nil
}

func (f *Flags) addInstanceFlags(inst *instance.Instance) {
	if len(inst.FeatureFlags) == 0 {
		return
	}
	m := make(map[string]interface{})
	for k, v := range inst.FeatureFlags {
		m[k] = v
	}
	flags := &Flags{
		DocID: consts.InstanceFlagsSettingsID,
		M:     m,
	}
	f.Sources = append(f.Sources, flags)
	for k, v := range flags.M {
		if _, ok := f.M[k]; !ok {
			f.M[k] = v
		}
	}
}

func (f *Flags) addManager(inst *instance.Instance) error {
	if len(inst.FeatureSets) == 0 {
		return nil
	}
	m, err := getFlagsFromManager(inst)
	if err != nil || len(m) == 0 {
		return err
	}
	flags := &Flags{
		DocID: consts.ManagerFlagsSettingsID,
		M:     m,
	}
	f.Sources = append(f.Sources, flags)
	for k, v := range flags.M {
		if _, ok := f.M[k]; !ok {
			f.M[k] = v
		}
	}
	return nil
}

var (
	cacheDuration      = 12 * time.Hour
	errInvalidResponse = errors.New("Invalid response from the manager")
)

func getFlagsFromManager(inst *instance.Instance) (map[string]interface{}, error) {
	cache := config.GetConfig().CacheStorage
	cacheKey := fmt.Sprintf("flags:%s:%v", inst.ContextName, inst.FeatureSets)
	var flags map[string]interface{}
	if buf, ok := cache.Get(cacheKey); ok {
		if err := json.Unmarshal(buf, &flags); err == nil {
			return flags, nil
		}
	}

	flags, err := fetchManagerFlags(inst)
	if err != nil || flags == nil {
		return flags, err
	}
	if buf, err := json.Marshal(flags); err == nil {
		cache.Set(cacheKey, buf, cacheDuration)
	}
	return flags, nil
}

func fetchManagerFlags(inst *instance.Instance) (map[string]interface{}, error) {
	client := instance.APIManagerClient(inst)
	if client == nil {
		return nil, nil
	}
	query := url.Values{
		"sets":    {strings.Join(inst.FeatureSets, ",")},
		"context": {inst.ContextName},
	}.Encode()
	data, err := client.Get(fmt.Sprintf("/api/v1/features?%s", query))
	if err != nil {
		return nil, err
	}
	flags, ok := data["flags"].(map[string]interface{})
	if !ok {
		return nil, errInvalidResponse
	}
	return flags, nil
}

func (f *Flags) addConfig(inst *instance.Instance) error {
	ctx, ok := inst.SettingsContext()
	if !ok {
		return nil
	}
	normalized := make(map[string]interface{})
	if m, ok := ctx["features"].(map[string]interface{}); ok {
		for k, v := range m {
			normalized[k] = v
		}
	} else if items, ok := ctx["features"].([]interface{}); ok {
		for _, item := range items {
			if m, ok := item.(map[string]interface{}); ok && len(m) == 1 {
				for k, v := range m {
					normalized[k] = v
				}
			} else {
				normalized[fmt.Sprintf("%v", item)] = true
			}
		}
	} else {
		return nil
	}
	ctxFlags := &Flags{
		DocID: consts.ConfigFlagsSettingsID,
		M:     normalized,
	}
	f.Sources = append(f.Sources, ctxFlags)
	for k, v := range ctxFlags.M {
		if _, ok := f.M[k]; !ok {
			f.M[k] = v
		}
	}
	return nil
}

func (f *Flags) addContext(inst *instance.Instance) error {
	id := fmt.Sprintf("%s.%s", consts.ContextFlagsSettingsID, inst.ContextName)
	var context Flags
	err := couchdb.GetDoc(prefixer.GlobalPrefixer, consts.Settings, id, &context)
	if couchdb.IsNotFoundError(err) {
		return nil
	} else if err != nil {
		return err
	}
	if len(context.M) == 0 {
		return nil
	}
	context.SetID(consts.ContextFlagsSettingsID)
	f.Sources = append(f.Sources, &context)
	for k, v := range context.M {
		if _, ok := f.M[k]; !ok {
			if value := applyRatio(inst, k, v); value != nil {
				f.M[k] = value
			}
		}
	}
	return nil
}

const maxUint32 = 1<<32 - 1

func applyRatio(inst *instance.Instance, key string, data interface{}) interface{} {
	items, ok := data.([]interface{})
	if !ok || len(items) == 0 {
		return nil
	}
	sum := crc32.ChecksumIEEE([]byte(fmt.Sprintf("%s:%s", inst.DocID, key)))
	for i := range items {
		item, ok := items[i].(map[string]interface{})
		if !ok {
			continue
		}
		ratio, ok := item["ratio"].(float64)
		if !ok || ratio == 0.0 {
			continue
		}
		if ratio == 1.0 {
			return item["value"]
		}
		computed := uint32(ratio * maxUint32)
		if computed >= sum {
			return item["value"]
		}
		sum -= computed
	}
	return nil
}

func (f *Flags) addDefaults(inst *instance.Instance) error {
	var defaults Flags
	err := couchdb.GetDoc(prefixer.GlobalPrefixer, consts.Settings, consts.DefaultFlagsSettingsID, &defaults)
	if couchdb.IsNotFoundError(err) {
		return nil
	} else if err != nil {
		return err
	}
	if len(defaults.M) == 0 {
		return nil
	}
	defaults.SetID(consts.DefaultFlagsSettingsID)
	f.Sources = append(f.Sources, &defaults)
	for k, v := range defaults.M {
		if _, ok := f.M[k]; !ok {
			f.M[k] = v
		}
	}
	return nil
}

var _ couchdb.Doc = &Flags{}
