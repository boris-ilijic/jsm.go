// Copyright 2020 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package jsm

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/jsm.go/api"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nuid"
)

// DefaultConsumer is the configuration that will be used to create new Consumers in NewConsumer
var DefaultConsumer = api.ConsumerConfig{
	DeliverPolicy: api.DeliverAll,
	AckPolicy:     api.AckExplicit,
	AckWait:       30 * time.Second,
	ReplayPolicy:  api.ReplayInstant,
}

// SampledDefaultConsumer is the configuration that will be used to create new Consumers in NewConsumer
var SampledDefaultConsumer = api.ConsumerConfig{
	DeliverPolicy:   api.DeliverAll,
	AckPolicy:       api.AckExplicit,
	AckWait:         30 * time.Second,
	ReplayPolicy:    api.ReplayInstant,
	SampleFrequency: "100%",
}

// ConsumerOption configures consumers
type ConsumerOption func(o *api.ConsumerConfig) error

// Consumer represents a JetStream consumer
type Consumer struct {
	name     string
	stream   string
	cfg      *api.ConsumerConfig
	mgr      *Manager
	lastInfo *api.ConsumerInfo

	sync.Mutex
}

// NewConsumerFromDefault creates a new consumer based on a template config that gets modified by opts
func (m *Manager) NewConsumerFromDefault(stream string, dflt api.ConsumerConfig, opts ...ConsumerOption) (consumer *Consumer, err error) {
	if !IsValidName(stream) {
		return nil, fmt.Errorf("%q is not a valid stream name", stream)
	}

	cfg, err := NewConsumerConfiguration(dflt, opts...)
	if err != nil {
		return nil, err
	}

	valid, errs := cfg.Validate()
	if !valid {
		return nil, fmt.Errorf("configuration validation failed: %s", strings.Join(errs, ", "))
	}

	// TODO: Remove this once natscli and the Terraform NATS provider are using update consumer
	// if we have a single filter subject in the array use the single filter string instead (which will then use the extended create request subject format)
	if len(cfg.FilterSubjects) == 1 {
		cfg.FilterSubject = cfg.FilterSubjects[0]
		cfg.FilterSubjects = nil
	}

	req := api.JSApiConsumerCreateRequest{
		Stream: stream,
		Config: *cfg,
	}

	createdInfo, err := m.createConsumer(req)
	if err != nil {
		return nil, err
	}

	if createdInfo == nil {
		return nil, fmt.Errorf("expected a consumer name but none were generated")
	}

	c := m.consumerFromCfg(stream, createdInfo.Name, &createdInfo.Config)
	c.lastInfo = createdInfo

	return c, nil
}

func (m *Manager) createConsumer(req api.JSApiConsumerCreateRequest) (info *api.ConsumerInfo, err error) {
	var resp api.JSApiConsumerCreateResponse

	if req.Config.Name == "" {
		return nil, fmt.Errorf("consumer conmfiguration requires a name")
	}

	var subj string
	if req.Config.FilterSubject == "" {
		subj = fmt.Sprintf(api.JSApiConsumerCreateWithNameT, req.Stream, req.Config.Name)
	} else {
		subj = fmt.Sprintf(api.JSApiConsumerCreateExT, req.Stream, req.Config.Name, req.Config.FilterSubject)
	}

	err = m.jsonRequest(subj, req, &resp)
	if err != nil {
		return nil, err
	}

	return resp.ConsumerInfo, nil
}

// NewConsumer creates a consumer based on DefaultConsumer modified by opts
func (m *Manager) NewConsumer(stream string, opts ...ConsumerOption) (consumer *Consumer, err error) {
	if !IsValidName(stream) {
		return nil, fmt.Errorf("%q is not a valid stream name", stream)
	}

	return m.NewConsumerFromDefault(stream, DefaultConsumer, opts...)
}

// LoadOrNewConsumer loads a consumer by name if known else creates a new one with these properties
func (m *Manager) LoadOrNewConsumer(stream string, name string, opts ...ConsumerOption) (consumer *Consumer, err error) {
	return m.LoadOrNewConsumerFromDefault(stream, name, DefaultConsumer, opts...)
}

// LoadOrNewConsumerFromDefault loads a consumer by name if known else creates a new one with these properties based on template
func (m *Manager) LoadOrNewConsumerFromDefault(stream string, name string, template api.ConsumerConfig, opts ...ConsumerOption) (consumer *Consumer, err error) {
	if !IsValidName(stream) {
		return nil, fmt.Errorf("%q is not a valid stream name", stream)
	}

	if !IsValidName(name) {
		return nil, fmt.Errorf("%q is not a valid consumer name", name)
	}

	c, err := m.LoadConsumer(stream, name)
	if IsNatsError(err, 10014) {
		return m.NewConsumerFromDefault(stream, template, opts...)
	}

	return c, err
}

// LoadConsumer loads a consumer by name
func (m *Manager) LoadConsumer(stream string, name string) (consumer *Consumer, err error) {
	if !IsValidName(stream) {
		return nil, fmt.Errorf("%q is not a valid stream name", stream)
	}

	if !IsValidName(name) {
		return nil, fmt.Errorf("%q is not a valid consumer name", name)
	}

	consumer = m.consumerFromCfg(stream, name, &api.ConsumerConfig{})

	err = m.loadConfigForConsumer(consumer)
	if err != nil {
		return nil, err
	}

	return consumer, nil
}

func (m *Manager) consumerFromCfg(stream string, name string, cfg *api.ConsumerConfig) *Consumer {
	if name == "" && cfg.Name != "" {
		name = cfg.Name
	}

	return &Consumer{
		name:   name,
		stream: stream,
		cfg:    cfg,
		mgr:    m,
	}
}

// NewConsumerConfiguration generates a new configuration based on template modified by opts
func NewConsumerConfiguration(dflt api.ConsumerConfig, opts ...ConsumerOption) (*api.ConsumerConfig, error) {
	cfg := dflt

	for _, o := range opts {
		err := o(&cfg)
		if err != nil {
			return nil, err
		}
	}

	if cfg.Durable != "" {
		cfg.Name = cfg.Durable
	}

	if cfg.Name == "" {
		cfg.Name = generateConsName()
	}

	return &cfg, nil
}

const rdigits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
const base = 62

func generateConsName() string {
	name := nuid.Next()
	sha := sha256.New()
	sha.Write([]byte(name))
	b := sha.Sum(nil)
	for i := 0; i < 8; i++ {
		b[i] = rdigits[int(b[i]%base)]
	}
	return string(b[:8])
}

func (m *Manager) loadConfigForConsumer(consumer *Consumer) (err error) {
	info, err := m.loadConsumerInfo(consumer.stream, consumer.name)
	if err != nil {
		return err
	}

	consumer.Lock()
	consumer.cfg = &info.Config
	consumer.lastInfo = &info
	consumer.Unlock()

	return nil
}

func (m *Manager) loadConsumerInfo(s string, c string) (info api.ConsumerInfo, err error) {
	var resp api.JSApiConsumerInfoResponse
	err = m.jsonRequest(fmt.Sprintf(api.JSApiConsumerInfoT, s, c), nil, &resp)
	if err != nil {
		return info, err
	}

	return *resp.ConsumerInfo, nil
}

// ConsumerDescription is a textual description of this consumer to provide additional context
func ConsumerDescription(d string) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		o.Description = d
		return nil
	}
}

// DeliverySubject is the subject where a Push consumer will deliver its messages
func DeliverySubject(s string) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		o.DeliverSubject = s
		return nil
	}
}

// ConsumerName sets a name for the consumer, when creating a durable consumer use DurableName, using ConsumerName allows
// for creating named ephemeral consumers, else a random name will be generated
func ConsumerName(s string) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		if !IsValidName(s) {
			return fmt.Errorf("%q is not a valid consumer name", s)
		}

		o.Name = s
		return nil
	}
}

// DurableName is the name given to the consumer, when not set an ephemeral consumer is created
func DurableName(s string) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		if !IsValidName(s) {
			return fmt.Errorf("%q is not a valid consumer name", s)
		}

		o.Durable = s
		return nil
	}
}

// StartAtSequence starts consuming messages at a specific sequence in the stream
func StartAtSequence(s uint64) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		resetDeliverPolicy(o)
		o.DeliverPolicy = api.DeliverByStartSequence
		o.OptStartSeq = s
		return nil
	}
}

// StartAtTime starts consuming messages at a specific point in time in the stream
func StartAtTime(t time.Time) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		resetDeliverPolicy(o)
		o.DeliverPolicy = api.DeliverByStartTime
		ut := t.UTC()
		o.OptStartTime = &ut
		return nil
	}
}

// DeliverAllAvailable delivers messages starting with the first available in the stream
func DeliverAllAvailable() ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		resetDeliverPolicy(o)
		o.DeliverPolicy = api.DeliverAll
		return nil
	}
}

// DeliverLastPerSubject delivers the last message for each subject in a wildcard stream based on the filter subjects of the consumer
func DeliverLastPerSubject() ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		resetDeliverPolicy(o)
		o.DeliverPolicy = api.DeliverLastPerSubject
		return nil
	}
}

// StartWithLastReceived starts delivery at the last messages received in the stream
func StartWithLastReceived() ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		resetDeliverPolicy(o)
		o.DeliverPolicy = api.DeliverLast
		return nil
	}
}

// StartWithNextReceived starts delivery at the next messages received in the stream
func StartWithNextReceived() ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		resetDeliverPolicy(o)
		o.DeliverPolicy = api.DeliverNew
		return nil
	}
}

// StartAtTimeDelta starts delivering messages at a past point in time
func StartAtTimeDelta(d time.Duration) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		resetDeliverPolicy(o)

		t := time.Now().UTC().Add(-1 * d)
		o.DeliverPolicy = api.DeliverByStartTime
		o.OptStartTime = &t
		return nil
	}
}

// DeliverHeadersOnly configures the consumer to only deliver existing header and the `Nats-Msg-Size` header, no bodies
func DeliverHeadersOnly() ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		o.HeadersOnly = true
		return nil
	}
}

func resetDeliverPolicy(o *api.ConsumerConfig) {
	o.DeliverPolicy = api.DeliverAll
	o.OptStartSeq = 0
	o.OptStartTime = nil
}

// AcknowledgeNone disables message acknowledgement
func AcknowledgeNone() ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		o.AckPolicy = api.AckNone
		return nil
	}
}

// AcknowledgeAll enables an acknowledgement mode where acknowledging message 100 will also ack the preceding messages
func AcknowledgeAll() ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		o.AckPolicy = api.AckAll
		return nil
	}
}

// AcknowledgeExplicit requires that every message received be acknowledged
func AcknowledgeExplicit() ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		o.AckPolicy = api.AckExplicit
		return nil
	}
}

// AckWait sets the time a delivered message might remain unacknowledged before redelivery is attempted
func AckWait(t time.Duration) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		o.AckWait = t
		return nil
	}
}

// MaxDeliveryAttempts is the number of times a message will be attempted to be delivered
func MaxDeliveryAttempts(n int) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		if n == 0 {
			return fmt.Errorf("configuration would prevent all deliveries")
		}
		o.MaxDeliver = n
		return nil
	}
}

// FilterStreamBySubject filters the messages in a wildcard stream to those matching a specific subject
func FilterStreamBySubject(s ...string) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		if len(s) == 1 {
			o.FilterSubject = s[0]
		} else {
			o.FilterSubjects = append(o.FilterSubjects, s...)
		}

		return nil
	}
}

// ReplayInstantly delivers messages to the consumer as fast as possible
func ReplayInstantly() ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		o.ReplayPolicy = api.ReplayInstant
		return nil
	}
}

// ReplayAsReceived delivers messages at the rate they were received at
func ReplayAsReceived() ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		o.ReplayPolicy = api.ReplayOriginal
		return nil
	}
}

// SamplePercent configures sampling of a subset of messages expressed as a percentage
func SamplePercent(i int) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		if i < 0 || i > 100 {
			return fmt.Errorf("sample percent must be 0-100")
		}

		if i == 0 {
			o.SampleFrequency = ""
			return nil
		}

		o.SampleFrequency = fmt.Sprintf("%d%%", i)
		return nil
	}
}

// RateLimitBitsPerSecond limits message delivery to a rate in bits per second
func RateLimitBitsPerSecond(bps uint64) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		o.RateLimit = bps
		return nil
	}
}

// MaxWaiting is the number of outstanding pulls that are allowed on any one consumer.  Pulls made that exceeds this limit are discarded.
func MaxWaiting(pulls uint) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		o.MaxWaiting = int(pulls)
		return nil
	}
}

// MaxAckPending maximum number of messages without acknowledgement that can be outstanding, once this limit is reached message delivery will be suspended
func MaxAckPending(pending uint) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		o.MaxAckPending = int(pending)
		return nil
	}
}

// IdleHeartbeat sets the time before an idle consumer will send a empty message with Status header 100 indicating the consumer is still alive
func IdleHeartbeat(hb time.Duration) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		o.Heartbeat = hb
		return nil
	}
}

// PushFlowControl enables flow control for push based consumers
func PushFlowControl() ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		o.FlowControl = true
		return nil
	}
}

// DeliverGroup when set will only deliver messages to subscriptions matching that group
func DeliverGroup(g string) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		o.DeliverGroup = g
		return nil
	}
}

// MaxRequestMaxBytes sets the limit of max bytes a consumer my request
func MaxRequestMaxBytes(max int) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		o.MaxRequestMaxBytes = max
		return nil
	}
}

// MaxRequestBatch is the largest batch that can be specified when doing pulls against the consumer
func MaxRequestBatch(max uint) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		o.MaxRequestBatch = int(max)
		return nil
	}
}

// MaxRequestExpires is the longest pull request expire the server will allow
func MaxRequestExpires(max time.Duration) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		if max != 0 && max < time.Millisecond {
			return fmt.Errorf("must be larger than 1ms")
		}

		o.MaxRequestExpires = max
		return nil
	}
}

// InactiveThreshold is the idle time an ephemeral consumer allows before it is removed
func InactiveThreshold(t time.Duration) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		if t < 0 {
			return fmt.Errorf("inactive threshold must be positive")
		}

		o.InactiveThreshold = t

		return nil
	}
}

// BackoffIntervals sets a series of intervals by which retries will be attempted for this consumr
func BackoffIntervals(i ...time.Duration) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		if len(i) == 0 {
			return fmt.Errorf("at least one interval is required")
		}

		o.BackOff = i

		return nil
	}
}

// BackoffPolicy sets a consumer policy
func BackoffPolicy(policy []time.Duration) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		o.BackOff = policy
		return nil
	}
}

// ConsumerOverrideReplicas override the replica count inherited from the Stream with this value
func ConsumerOverrideReplicas(r int) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		o.Replicas = r
		return nil
	}
}

func ConsumerOverrideMemoryStorage() ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		o.MemoryStorage = true
		return nil
	}
}

// LinearBackoffPolicy creates a backoff policy with linearly increasing steps between min and max
func LinearBackoffPolicy(steps uint, min time.Duration, max time.Duration) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		p, err := LinearBackoffPeriods(steps, min, max)
		if err != nil {
			return err
		}

		o.BackOff = p

		return nil
	}
}

func ConsumerMetadata(meta map[string]string) ConsumerOption {
	return func(o *api.ConsumerConfig) error {
		for k := range meta {
			if len(k) == 0 {
				return fmt.Errorf("invalid empty string key in metadata")
			}
		}

		o.Metadata = meta
		return nil
	}
}

// UpdateConfiguration updates the consumer configuration
// At present the description, ack wait, max deliver, sample frequency, max ack pending, max waiting and header only settings can be changed
func (c *Consumer) UpdateConfiguration(opts ...ConsumerOption) error {
	if !c.IsDurable() {
		return fmt.Errorf("only durable consumers can be updated")
	}

	ncfg, err := NewConsumerConfiguration(*c.cfg, opts...)
	if err != nil {
		return err
	}

	_, err = c.mgr.NewConsumerFromDefault(c.stream, *ncfg)
	if err != nil {
		return err
	}

	return c.Reset()
}

// Reset reloads the Consumer configuration from the JetStream server
func (c *Consumer) Reset() error {
	return c.mgr.loadConfigForConsumer(c)
}

// NextSubject returns the subject used to retrieve the next message for pull-based Consumers, empty when not a pull-base consumer
func (m *Manager) NextSubject(stream string, consumer string) (string, error) {
	s, err := NextSubject(stream, consumer)
	if err != nil {
		return "", err
	}

	return m.apiSubject(s), err
}

// NextSubject returns the subject used to retrieve the next message for pull-based Consumers, empty when not a pull-base consumer
func (c *Consumer) NextSubject() string {
	if !c.IsPullMode() {
		return ""
	}

	s, _ := c.mgr.NextSubject(c.stream, c.name)

	return s
}

// NextSubject returns the subject used to retrieve the next message for pull-based Consumers, empty when not a pull-base consumer
func NextSubject(stream string, consumer string) (string, error) {
	if !IsValidName(stream) {
		return "", fmt.Errorf("%q is not a valid stream name", stream)
	}
	if !IsValidName(consumer) {
		return "", fmt.Errorf("%q is not a valid consumer name", consumer)
	}

	return fmt.Sprintf(api.JSApiRequestNextT, stream, consumer), nil
}

// AckSampleSubject is the subject used to publish ack samples to
func (c *Consumer) AckSampleSubject() string {
	if c.SampleFrequency() == "" {
		return ""
	}

	return api.JSMetricConsumerAckPre + "." + c.StreamName() + "." + c.name
}

// AdvisorySubject is a wildcard subscription subject that subscribes to all advisories for this consumer
func (c *Consumer) AdvisorySubject() string {
	return api.JSAdvisoryPrefix + ".CONSUMER.*." + c.StreamName() + "." + c.name
}

// MetricSubject is a wildcard subscription subject that subscribes to all metrics for this consumer
func (c *Consumer) MetricSubject() string {
	return api.JSMetricPrefix + ".CONSUMER.*." + c.StreamName() + "." + c.name
}

// NextMsg requests the next message from the server with the manager timeout
func (m *Manager) NextMsg(stream string, consumer string) (*nats.Msg, error) {
	if !m.nc.Opts.UseOldRequestStyle {
		return nil, fmt.Errorf("pull mode requires the use of UseOldRequestStyle() option")
	}

	s, err := m.NextSubject(stream, consumer)
	if err != nil {
		return nil, err
	}

	rj, err := json.Marshal(&api.JSApiConsumerGetNextRequest{
		Expires: m.timeout,
		Batch:   1,
	})
	if err != nil {
		return nil, err
	}

	return m.request(s, rj)
}

// NextMsgRequest creates a request for a batch of messages on a consumer, data or control flow messages will be sent to inbox
func (m *Manager) NextMsgRequest(stream string, consumer string, inbox string, req *api.JSApiConsumerGetNextRequest) error {
	s, err := m.NextSubject(stream, consumer)
	if err != nil {
		return err
	}

	jreq, err := json.Marshal(req)
	if err != nil {
		return err
	}

	if m.trace {
		log.Printf(">>> %s:\n%s\n\n", s, string(jreq))
	}

	return m.nc.PublishMsg(&nats.Msg{Subject: s, Reply: inbox, Data: jreq})
}

// NextMsgContext requests the next message from the server. This request will wait for as long as the context is
// active. If repeated pulls will be made it's better to use NextMsgRequest()
func (m *Manager) NextMsgContext(ctx context.Context, stream string, consumer string) (*nats.Msg, error) {
	if !m.nc.Opts.UseOldRequestStyle {
		return nil, fmt.Errorf("pull mode requires the use of UseOldRequestStyle() option")
	}

	s, err := m.NextSubject(stream, consumer)
	if err != nil {
		return nil, err
	}

	return m.requestWithContext(ctx, s, []byte(strconv.Itoa(1)))
}

// NextMsgRequest creates a request for a batch of messages, data or control flow messages will be sent to inbox
func (c *Consumer) NextMsgRequest(inbox string, req *api.JSApiConsumerGetNextRequest) error {
	return c.mgr.NextMsgRequest(c.stream, c.name, inbox, req)
}

// NextMsg retrieves the next message, waiting up to manager timeout for a response
func (c *Consumer) NextMsg() (*nats.Msg, error) {
	return c.mgr.NextMsg(c.stream, c.name)
}

// NextMsgContext retrieves the next message, interrupted by the cancel context ctx
func (c *Consumer) NextMsgContext(ctx context.Context) (*nats.Msg, error) {
	return c.mgr.NextMsgContext(ctx, c.stream, c.name)
}

// DeliveredState reports the messages sequences that were successfully delivered
func (c *Consumer) DeliveredState() (api.SequenceInfo, error) {
	info, err := c.State()
	if err != nil {
		return api.SequenceInfo{}, err
	}

	return info.Delivered, nil
}

// AcknowledgedFloor reports the highest contiguous message sequences that were acknowledged
func (c *Consumer) AcknowledgedFloor() (api.SequenceInfo, error) {
	info, err := c.State()
	if err != nil {
		return api.SequenceInfo{}, err
	}

	return info.AckFloor, nil
}

// PendingAcknowledgement reports the number of messages sent but not yet acknowledged
func (c *Consumer) PendingAcknowledgement() (int, error) {
	info, err := c.State()
	if err != nil {
		return 0, err
	}

	return info.NumAckPending, nil
}

// PendingMessages is the number of unprocessed messages for this consumer
func (c *Consumer) PendingMessages() (uint64, error) {
	info, err := c.State()
	if err != nil {
		return 0, err
	}

	return info.NumPending, nil
}

// WaitingClientPulls is the number of clients that have outstanding pull requests against this consumer
func (c *Consumer) WaitingClientPulls() (int, error) {
	info, err := c.State()
	if err != nil {
		return 0, err
	}

	return info.NumWaiting, nil
}

// RedeliveryCount reports the number of redelivers that were done
func (c *Consumer) RedeliveryCount() (int, error) {
	info, err := c.State()
	if err != nil {
		return 0, err
	}

	return info.NumRedelivered, nil
}

// LatestState returns the most recently loaded state
func (c *Consumer) LatestState() (api.ConsumerInfo, error) {
	c.Lock()
	s := c.lastInfo
	c.Unlock()

	if s != nil {
		return *s, nil
	}

	return c.State()
}

// State loads a snapshot of consumer state including delivery counts, retries and more
func (c *Consumer) State() (api.ConsumerInfo, error) {
	s, err := c.mgr.loadConsumerInfo(c.stream, c.name)
	if err != nil {
		return api.ConsumerInfo{}, err
	}

	c.Lock()
	c.lastInfo = &s
	c.Unlock()

	return s, nil
}

// Configuration is the Consumer configuration
func (c *Consumer) Configuration() (config api.ConsumerConfig) {
	return *c.cfg
}

// Delete deletes the Consumer, after this the Consumer object should be disposed
func (c *Consumer) Delete() (err error) {
	var resp api.JSApiConsumerDeleteResponse
	err = c.mgr.jsonRequest(fmt.Sprintf(api.JSApiConsumerDeleteT, c.StreamName(), c.Name()), nil, &resp)
	if err != nil {
		return err
	}

	if resp.Success {
		return nil
	}

	return fmt.Errorf("unknown response while removing consumer %s", c.Name())
}

// LeaderStepDown requests the current RAFT group leader in a clustered JetStream to stand down forcing a new election
func (c *Consumer) LeaderStepDown() error {
	var resp api.JSApiConsumerLeaderStepDownResponse
	err := c.mgr.jsonRequest(fmt.Sprintf(api.JSApiConsumerLeaderStepDownT, c.StreamName(), c.Name()), nil, &resp)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("unknown error while requesting leader step down")
	}

	return nil
}

func (c *Consumer) Name() string                     { return c.name }
func (c *Consumer) IsSampled() bool                  { return c.SampleFrequency() != "" }
func (c *Consumer) IsPullMode() bool                 { return c.cfg.DeliverSubject == "" }
func (c *Consumer) IsPushMode() bool                 { return !c.IsPullMode() }
func (c *Consumer) IsDurable() bool                  { return c.cfg.Durable != "" }
func (c *Consumer) IsEphemeral() bool                { return !c.IsDurable() }
func (c *Consumer) IsHeadersOnly() bool              { return c.cfg.HeadersOnly }
func (c *Consumer) StreamName() string               { return c.stream }
func (c *Consumer) DeliverySubject() string          { return c.cfg.DeliverSubject }
func (c *Consumer) DurableName() string              { return c.cfg.Durable }
func (c *Consumer) Description() string              { return c.cfg.Description }
func (c *Consumer) StartSequence() uint64            { return c.cfg.OptStartSeq }
func (c *Consumer) DeliverPolicy() api.DeliverPolicy { return c.cfg.DeliverPolicy }
func (c *Consumer) AckPolicy() api.AckPolicy         { return c.cfg.AckPolicy }
func (c *Consumer) AckWait() time.Duration           { return c.cfg.AckWait }
func (c *Consumer) MaxDeliver() int                  { return c.cfg.MaxDeliver }
func (c *Consumer) Backoff() []time.Duration         { return c.cfg.BackOff }
func (c *Consumer) FilterSubject() string            { return c.cfg.FilterSubject }
func (c *Consumer) FilterSubjects() []string         { return c.cfg.FilterSubjects }
func (c *Consumer) ReplayPolicy() api.ReplayPolicy   { return c.cfg.ReplayPolicy }
func (c *Consumer) SampleFrequency() string          { return c.cfg.SampleFrequency }
func (c *Consumer) RateLimit() uint64                { return c.cfg.RateLimit }
func (c *Consumer) MaxAckPending() int               { return c.cfg.MaxAckPending }
func (c *Consumer) FlowControl() bool                { return c.cfg.FlowControl }
func (c *Consumer) Heartbeat() time.Duration         { return c.cfg.Heartbeat }
func (c *Consumer) DeliverGroup() string             { return c.cfg.DeliverGroup }
func (c *Consumer) MaxWaiting() int                  { return c.cfg.MaxWaiting }
func (c *Consumer) MaxRequestBatch() int             { return c.cfg.MaxRequestBatch }
func (c *Consumer) MaxRequestExpires() time.Duration { return c.cfg.MaxRequestExpires }
func (c *Consumer) MaxRequestMaxBytes() int          { return c.cfg.MaxRequestMaxBytes }
func (c *Consumer) InactiveThreshold() time.Duration { return c.cfg.InactiveThreshold }
func (c *Consumer) Replicas() int                    { return c.cfg.Replicas }
func (c *Consumer) Metadata() map[string]string      { return c.cfg.Metadata }
func (c *Consumer) MemoryStorage() bool              { return c.cfg.MemoryStorage }
func (c *Consumer) StartTime() time.Time {
	if c.cfg.OptStartTime == nil {
		return time.Time{}
	}
	return *c.cfg.OptStartTime
}
