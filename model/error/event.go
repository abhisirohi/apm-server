// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package error

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
	"time"

	"github.com/santhosh-tekuri/jsonschema"

	m "github.com/elastic/apm-server/model"
	"github.com/elastic/apm-server/model/error/generated/schema"
	"github.com/elastic/apm-server/model/metadata"
	"github.com/elastic/apm-server/transform"
	"github.com/elastic/apm-server/utility"
	"github.com/elastic/apm-server/validation"
	"github.com/elastic/beats/libbeat/beat"
	"github.com/elastic/beats/libbeat/common"
	"github.com/elastic/beats/libbeat/monitoring"
)

var (
	Metrics           = monitoring.Default.NewRegistry("apm-server.processor.error", monitoring.PublishExpvar)
	transformations   = monitoring.NewInt(Metrics, "transformations")
	stacktraceCounter = monitoring.NewInt(Metrics, "stacktraces")
	frameCounter      = monitoring.NewInt(Metrics, "frames")
	processorEntry    = common.MapStr{"name": processorName, "event": errorDocType}
)

const (
	processorName = "error"
	errorDocType  = "error"
)

var cachedModelSchema = validation.CreateSchema(schema.ModelSchema, processorName)

func ModelSchema() *jsonschema.Schema {
	return cachedModelSchema
}

type Event struct {
	Id            *string
	TransactionId *string
	TraceId       *string
	ParentId      *string

	Timestamp time.Time

	Culprit *string
	User    *metadata.User
	Labels  *m.Labels
	Page    *m.Page
	Http    *m.Http
	Url     *m.Url
	Custom  *m.Custom
	Service *metadata.Service

	Exception *Exception
	Log       *Log

	TransactionSampled *bool
	TransactionType    *string

	Experimental interface{}
	data         common.MapStr
}

type Exception struct {
	Message    *string
	Module     *string
	Code       interface{}
	Attributes interface{}
	Stacktrace m.Stacktrace
	Type       *string
	Handled    *bool
}

type Log struct {
	Message      string
	Level        *string
	ParamMessage *string
	LoggerName   *string
	Stacktrace   m.Stacktrace
}

func DecodeEvent(input interface{}, cfg m.Config, err error) (transform.Transformable, error) {
	if err != nil {
		return nil, err
	}
	if input == nil {
		return nil, errors.New("Input missing for decoding Event")
	}

	raw, ok := input.(map[string]interface{})
	if !ok {
		return nil, errors.New("invalid type for error event")
	}

	ctx, err := m.DecodeContext(raw, cfg, nil)
	if err != nil {
		return nil, err
	}
	decoder := utility.ManualDecoder{}
	e := Event{
		Id:                 decoder.StringPtr(raw, "id"),
		Culprit:            decoder.StringPtr(raw, "culprit"),
		Labels:             ctx.Labels,
		Page:               ctx.Page,
		Http:               ctx.Http,
		Url:                ctx.Url,
		Custom:             ctx.Custom,
		User:               ctx.User,
		Service:            ctx.Service,
		Experimental:       ctx.Experimental,
		Timestamp:          decoder.TimeEpochMicro(raw, "timestamp"),
		TransactionId:      decoder.StringPtr(raw, "transaction_id"),
		ParentId:           decoder.StringPtr(raw, "parent_id"),
		TraceId:            decoder.StringPtr(raw, "trace_id"),
		TransactionSampled: decoder.BoolPtr(raw, "sampled", "transaction"),
		TransactionType:    decoder.StringPtr(raw, "type", "transaction"),
	}

	var stacktr *m.Stacktrace
	ex := decoder.MapStr(raw, "exception")
	exMsg := decoder.StringPtr(ex, "message")
	exType := decoder.StringPtr(ex, "type")
	if exMsg != nil || exType != nil {
		e.Exception = &Exception{
			Message:    exMsg,
			Type:       exType,
			Code:       decoder.Interface(ex, "code"),
			Module:     decoder.StringPtr(ex, "module"),
			Attributes: decoder.Interface(ex, "attributes"),
			Handled:    decoder.BoolPtr(ex, "handled"),
			Stacktrace: m.Stacktrace{},
		}
		stacktr, decoder.Err = m.DecodeStacktrace(ex["stacktrace"], decoder.Err)
		if stacktr != nil {
			e.Exception.Stacktrace = *stacktr
		}
	}

	log := decoder.MapStr(raw, "log")
	logMsg := decoder.StringPtr(log, "message")
	if logMsg != nil {
		e.Log = &Log{
			Message:      *logMsg,
			ParamMessage: decoder.StringPtr(log, "param_message"),
			Level:        decoder.StringPtr(log, "level"),
			LoggerName:   decoder.StringPtr(log, "logger_name"),
			Stacktrace:   m.Stacktrace{},
		}
		stacktr, decoder.Err = m.DecodeStacktrace(log["stacktrace"], decoder.Err)
		if stacktr != nil {
			e.Log.Stacktrace = *stacktr
		}
	}
	if decoder.Err != nil {
		return nil, decoder.Err
	}

	return &e, nil
}

func (e *Event) Transform(tctx *transform.Context) []beat.Event {
	transformations.Inc()

	if e.Exception != nil {
		addStacktraceCounter(e.Exception.Stacktrace)
	}
	if e.Log != nil {
		addStacktraceCounter(e.Log.Stacktrace)
	}

	fields := common.MapStr{
		"error":     e.fields(tctx),
		"processor": processorEntry,
	}

	// first set the generic metadata (order is relevant)
	tctx.Metadata.Set(fields)
	// then add event specific information
	utility.Update(fields, "user", e.User.Fields())
	utility.DeepUpdate(fields, "client", e.User.ClientFields())
	utility.DeepUpdate(fields, "user_agent", e.User.UserAgentFields())
	utility.DeepUpdate(fields, "service", e.Service.Fields())
	utility.DeepUpdate(fields, "agent", e.Service.AgentFields())
	// merges with metadata labels, overrides conflicting keys
	utility.DeepUpdate(fields, "labels", e.Labels.Fields())
	utility.Set(fields, "http", e.Http.Fields())
	utility.Set(fields, "url", e.Url.Fields())
	utility.Set(fields, "experimental", e.Experimental)

	// sampled and type is nil if an error happens outside a transaction or an (old) agent is not sending sampled info
	// agents must send semantically correct data
	if e.TransactionSampled != nil || e.TransactionType != nil || (e.TransactionId != nil && *e.TransactionId != "") {
		transaction := common.MapStr{}
		utility.Set(transaction, "id", e.TransactionId)
		utility.Set(transaction, "type", e.TransactionType)
		utility.Set(transaction, "sampled", e.TransactionSampled)
		utility.Set(fields, "transaction", transaction)
	}

	utility.AddId(fields, "parent", e.ParentId)
	utility.AddId(fields, "trace", e.TraceId)

	if e.Timestamp.IsZero() {
		e.Timestamp = tctx.RequestTime
	}
	utility.Set(fields, "timestamp", utility.TimeAsMicros(e.Timestamp))

	return []beat.Event{
		{
			Fields:    fields,
			Timestamp: e.Timestamp,
		},
	}
}

func (e *Event) fields(tctx *transform.Context) common.MapStr {
	e.data = common.MapStr{}
	e.add("id", e.Id)
	e.add("page", e.Page.Fields())

	e.addException(tctx)
	e.addLog(tctx)

	e.updateCulprit(tctx)
	e.add("culprit", e.Culprit)
	e.add("custom", e.Custom.Fields())

	e.addGroupingKey()

	return e.data
}

func (e *Event) updateCulprit(tctx *transform.Context) {
	if tctx.Config.SmapMapper == nil {
		return
	}
	var fr *m.StacktraceFrame
	if e.Log != nil {
		fr = findSmappedNonLibraryFrame(e.Log.Stacktrace)
	}
	if fr == nil && e.Exception != nil {
		fr = findSmappedNonLibraryFrame(e.Exception.Stacktrace)
	}
	if fr == nil {
		return
	}
	culprit := fmt.Sprintf("%v", fr.Filename)
	if fr.Function != nil {
		culprit += fmt.Sprintf(" in %v", *fr.Function)
	}
	e.Culprit = &culprit
}

func findSmappedNonLibraryFrame(frames []*m.StacktraceFrame) *m.StacktraceFrame {
	for _, fr := range frames {
		if fr.IsSourcemapApplied() && !fr.IsLibraryFrame() {
			return fr
		}
	}
	return nil
}

func (e *Event) addException(tctx *transform.Context) {
	if e.Exception == nil {
		return
	}
	ex := common.MapStr{}
	utility.Set(ex, "message", e.Exception.Message)
	utility.Set(ex, "module", e.Exception.Module)
	utility.Set(ex, "attributes", e.Exception.Attributes)
	utility.Set(ex, "type", e.Exception.Type)
	utility.Set(ex, "handled", e.Exception.Handled)

	switch code := e.Exception.Code.(type) {
	case int:
		utility.Set(ex, "code", strconv.Itoa(code))
	case float64:
		utility.Set(ex, "code", fmt.Sprintf("%.0f", code))
	case string:
		utility.Set(ex, "code", code)
	case json.Number:
		utility.Set(ex, "code", code.String())
	}

	st := e.Exception.Stacktrace.Transform(tctx)
	utility.Set(ex, "stacktrace", st)

	// NOTE(axw) error.exception is an array of objects.
	// For now, the array holds just one exception. Later,
	// the array will hold each of the elements of a chained
	// exception, starting with the outermost exception and
	// ending with the root cause.
	e.add("exception", []common.MapStr{ex})
}

func (e *Event) addLog(tctx *transform.Context) {
	if e.Log == nil {
		return
	}
	log := common.MapStr{}
	utility.Set(log, "message", e.Log.Message)
	utility.Set(log, "param_message", e.Log.ParamMessage)
	utility.Set(log, "logger_name", e.Log.LoggerName)
	utility.Set(log, "level", e.Log.Level)
	st := e.Log.Stacktrace.Transform(tctx)
	utility.Set(log, "stacktrace", st)

	e.add("log", log)
}

func (e *Event) addGroupingKey() {
	e.add("grouping_key", e.calcGroupingKey())
}

type groupingKey struct {
	hash  hash.Hash
	empty bool
}

func newGroupingKey() *groupingKey {
	return &groupingKey{
		hash:  md5.New(),
		empty: true,
	}
}

func (k *groupingKey) add(s *string) bool {
	if s == nil {
		return false
	}
	io.WriteString(k.hash, *s)
	k.empty = false
	return true
}

func (k *groupingKey) addEither(s1 *string, s2 string) {
	if ok := k.add(s1); !ok {
		k.add(&s2)
	}
}

func (k *groupingKey) String() string {
	return hex.EncodeToString(k.hash.Sum(nil))
}

// calcGroupingKey computes a value for deduplicating errors - events with
// same grouping key can be collapsed together.
func (e *Event) calcGroupingKey() string {
	k := newGroupingKey()

	var st m.Stacktrace
	if e.Exception != nil {
		k.add(e.Exception.Type)
		st = e.Exception.Stacktrace
	}
	if e.Log != nil {
		k.add(e.Log.ParamMessage)
		if st == nil || len(st) == 0 {
			st = e.Log.Stacktrace
		}
	}

	for _, fr := range st {
		if fr.ExcludeFromGrouping {
			continue
		}
		k.addEither(fr.Module, fr.Filename)
		k.addEither(fr.Function, string(fr.Lineno))
	}
	if k.empty {
		if e.Exception != nil {
			k.add(e.Exception.Message)
		} else if e.Log != nil {
			k.add(&e.Log.Message)
		}
	}

	return k.String()
}

func (e *Event) add(key string, val interface{}) {
	utility.Set(e.data, key, val)
}

func addStacktraceCounter(st m.Stacktrace) {
	if frames := len(st); frames > 0 {
		stacktraceCounter.Inc()
		frameCounter.Add(int64(frames))
	}
}
