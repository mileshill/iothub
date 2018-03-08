package iotservice

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/amenzhinsky/golang-iothub/common"
	"github.com/amenzhinsky/golang-iothub/common/commonamqp"
	"github.com/amenzhinsky/golang-iothub/eventhub"
	"pack.ag/amqp"
)

// ClientOption is a client connectivity option.
type ClientOption func(c *Client) error

// WithConnectionString parses the given connection string instead of using `WithCredentials`.
func WithConnectionString(cs string) ClientOption {
	return func(c *Client) error {
		creds, err := common.ParseConnectionString(cs)
		if err != nil {
			return err
		}
		c.creds = creds
		return nil
	}
}

// WithCredentials uses the given credentials to generate SAS tokens.
func WithCredentials(creds *common.Credentials) ClientOption {
	return func(c *Client) error {
		c.creds = creds
		return nil
	}
}

// WithHTTPClient changes default http rest client.
func WithHTTPClient(client *http.Client) ClientOption {
	return func(c *Client) error {
		c.http = client
		return nil
	}
}

// WithLogger sets client logger.
func WithLogger(l *log.Logger) ClientOption {
	return func(c *Client) error {
		c.logger = l
		return nil
	}
}

// WithDebug enables or disables debug mode.
func WithDebug(d bool) ClientOption {
	return func(c *Client) error {
		c.debug = d
		return nil
	}
}

// NewClient creates new iothub service client.
func NewClient(opts ...ClientOption) (*Client, error) {
	c := &Client{
		done: make(chan struct{}),
	}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}

	if c.creds == nil {
		return nil, errors.New("credentials are missing, consider using `WithCredentials` or `WithConnectionString` option")
	}

	// set the default rest client, it uses only bundled ca-certificates
	// it's useful when the ca-certificates package is not present on
	// a very slim host systems like alpine or busybox.
	if c.http == nil {
		c.http = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					RootCAs: common.RootCAs(),
				},
			},
		}
	}
	return c, nil
}

type Client struct {
	mu     sync.Mutex
	conn   *eventhub.Client
	done   chan struct{}
	creds  *common.Credentials
	logger *log.Logger
	debug  bool
	http   *http.Client // REST client
}

// Connect connects to AMQP broker, has to be done before publishing events.
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	eh, err := eventhub.Dial(c.creds.HostName, &tls.Config{
		ServerName: c.creds.HostName,
		RootCAs:    common.RootCAs(),
	})
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			eh.Close()
		}
	}()

	sas, err := c.creds.SAS(c.creds.HostName, time.Hour)
	if err != nil {
		return err
	}
	if err = eh.PutTokenContinuously(ctx, c.creds.HostName, sas, c.done); err != nil {
		return err
	}
	c.conn = eh
	return nil
}

// C2D used two absolutely different ways of authentication for sending
// messages and subscribing to events stream.
//
// In this case we connect to an eventhub instance to listen to events.
func (c *Client) connectToEventHub(ctx context.Context) (*amqp.Client, string, error) {
	user := c.creds.SharedAccessKeyName + "@sas.root." + c.creds.HostName
	user = user[:len(user)-18] // sub .azure-devices.net"
	pass, err := c.creds.SAS(c.creds.HostName, time.Hour)
	if err != nil {
		return nil, "", err
	}

	addr := "amqps://" + c.creds.HostName
	conn, err := amqp.Dial(addr, amqp.ConnSASLPlain(user, pass))
	if err != nil {
		return nil, "", err
	}
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()

	sess, err := conn.NewSession()
	if err != nil {
		return nil, "", err
	}
	defer sess.Close()

	// trigger redirect error
	recv, err := sess.NewReceiver(amqp.LinkSourceAddress("messages/events/"))
	if err != nil {
		return nil, "", err
	}
	defer recv.Close()
	_, err = recv.Receive(ctx)

	if err == nil {
		return nil, "", errors.New("expected redirect error")
	}

	rerr, ok := err.(amqp.DetachError)
	if !ok || rerr.RemoteError.Condition != amqp.ErrorLinkRedirect {
		return nil, "", err
	}

	// "amqps://{host}:5671/{consumerGroup}/"
	group := rerr.RemoteError.Info["address"].(string)
	group = group[strings.Index(group, ":5671/")+6 : len(group)-1]

	addr = "amqps://" + rerr.RemoteError.Info["hostname"].(string)
	conn, err = amqp.Dial(addr, amqp.ConnSASLPlain(c.creds.SharedAccessKeyName, c.creds.SharedAccessKey))
	if err != nil {
		return nil, "", err
	}
	return conn, group, nil
}

func (c *Client) isConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

var errNotConnected = errors.New("not connected")

// SubscribeFunc handles incoming cloud-to-device events.
type SubscribeFunc func(e *common.Message)

// SubscribeEvents subscribes to device events.
// No need to call Connect first, because this method different connect
// method that dials an eventhub instance first opposed to SendEvent func.
func (c *Client) SubscribeEvents(ctx context.Context, f SubscribeFunc) error {
	conn, group, err := c.connectToEventHub(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	sess, err := conn.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()

	return eventhub.SubscribePartitions(ctx, sess, group, "$Default", func(msg *amqp.Message) {
		go f(commonamqp.FromAMQPMessage(msg))
	})
}

// SendOption is a send option.
type SendOption func(msg *common.Message) error

// WithSendMessageID sets message id.
func WithSendMessageID(mid string) SendOption {
	return func(msg *common.Message) error {
		msg.MessageID = mid
		return nil
	}
}

// WithSendCorrelationID sets correlation id.
func WithSendCorrelationID(cid string) SendOption {
	return func(msg *common.Message) error {
		msg.CorrelationID = cid
		return nil
	}
}

// WithSendUserID sets user id.
func WithSendUserID(uid string) SendOption {
	return func(msg *common.Message) error {
		msg.UserID = uid
		return nil
	}
}

const (
	// AckNone no feedback.
	AckNone = "none"

	// AckPositive receive a feedback message if the message was completed.
	AckPositive = "positive"

	// AckNegative receive a feedback message if the message expired
	// (or maximum delivery count was reached) without being completed by the device.
	AckNegative = "negative"

	// AckFull both positive and negative.
	AckFull = "full"
)

// WithSendAck sets message confirmation type.
func WithSendAck(typ string) SendOption {
	return func(msg *common.Message) error {
		switch typ {
		case "", AckNone, AckPositive, AckNegative, AckFull:
		default:
			return fmt.Errorf("unknown ack type: %q", typ)
		}
		return WithSendProperty("iothub-ack", typ)(msg)
	}
}

// WithSentExpiryTime sets message expiration time.
func WithSentExpiryTime(t time.Time) SendOption {
	return func(msg *common.Message) error {
		msg.ExpiryTime = t
		return nil
	}
}

// WithSendProperty sets a message property.
func WithSendProperty(k, v string) SendOption {
	return func(msg *common.Message) error {
		if msg.Properties == nil {
			msg.Properties = map[string]string{}
		}
		msg.Properties[k] = v
		return nil
	}
}

// WithSendProperties same as `WithSendProperty` but accepts map of keys and values.
func WithSendProperties(m map[string]string) SendOption {
	return func(msg *common.Message) error {
		if msg.Properties == nil {
			msg.Properties = map[string]string{}
		}
		for k, v := range m {
			msg.Properties[k] = v
		}
		return nil
	}
}

// SendEvent sends the given cloud-to-device message and returns its id.
// Panics when event is nil.
func (c *Client) SendEvent(
	ctx context.Context,
	deviceID string,
	payload []byte,
	opts ...SendOption,
) error {
	if deviceID == "" {
		return errors.New("device id is empty")
	}
	if payload == nil {
		return errors.New("payload is nil")
	}

	if !c.isConnected() {
		return errNotConnected
	}

	msg := &common.Message{
		Payload: payload,
		To:      "/devices/" + deviceID + "/messages/devicebound",
	}
	for _, opt := range opts {
		if err := opt(msg); err != nil {
			return err
		}
	}

	// opening a new link for every message is not the most efficient way
	send, err := c.conn.Sess().NewSender(
		amqp.LinkTargetAddress("/messages/devicebound"),
	)
	if err != nil {
		return err
	}
	defer send.Close()
	return send.Send(ctx, commonamqp.ToAMQPMessage(msg))
}

// FeedbackFunc handles message feedback.
type FeedbackFunc func(f *Feedback)

// SubscribeFeedback subscribes to feedback of messages that ack was requested.
func (c *Client) SubscribeFeedback(ctx context.Context, fn FeedbackFunc) error {
	if !c.isConnected() {
		return errNotConnected
	}
	recv, err := c.conn.Sess().NewReceiver(
		amqp.LinkSourceAddress("/messages/servicebound/feedback"),
	)
	if err != nil {
		return err
	}
	defer recv.Close()

	for {
		msg, err := recv.Receive(ctx)
		if err != nil {
			return err
		}
		msg.Accept()

		var v []*Feedback
		if err = json.Unmarshal(msg.Data[0], &v); err != nil {
			return err
		}
		for _, f := range v {
			go fn(f)
		}
	}
}

// Feedback is message feedback.
type Feedback struct {
	OriginalMessageID  string    `json:"originalMessageId"`
	Description        string    `json:"description"`
	DeviceGenerationID string    `json:"deviceGenerationId"`
	DeviceID           string    `json:"deviceId"`
	EnqueuedTimeUTC    time.Time `json:"enqueuedTimeUtc"`
	StatusCode         string    `json:"statusCode"`
}

type call struct {
	MethodName      string                 `json:"methodName"`
	ConnectTimeout  int                    `json:"connectTimeoutInSeconds,omitempty"`
	ResponseTimeout int                    `json:"responseTimeoutInSeconds,omitempty"`
	Payload         map[string]interface{} `json:"payload"`
}

// CallOption is a direct-method invocation option.
type CallOption func(c *call) error

// ConnectTimeout is connection timeout in seconds.
func WithCallConnectTimeout(seconds int) CallOption {
	return func(c *call) error {
		c.ConnectTimeout = seconds
		return nil
	}
}

// ResponseTimeout is response timeout in seconds.
func WithCallResponseTimeout(seconds int) CallOption {
	return func(c *call) error {
		c.ResponseTimeout = seconds
		return nil
	}
}

// Call calls the named direct method on with the given parameters.
func (c *Client) Call(
	ctx context.Context,
	deviceID string,
	methodName string,
	payload map[string]interface{},
	opts ...CallOption,
) (map[string]interface{}, error) {
	if deviceID == "" {
		return nil, errors.New("deviceID is empty")
	}
	if methodName == "" {
		return nil, errors.New("methodName is empty")
	}
	if len(payload) == 0 {
		return nil, errors.New("payload is empty")
	}

	v := &call{
		MethodName: methodName,
		Payload:    payload,
	}
	for _, opt := range opts {
		if err := opt(v); err != nil {
			return nil, err
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}

	b, err = c.request(ctx, http.MethodPost, "twins/%s/methods", deviceID, b)
	if err != nil {
		return nil, err
	}

	var ir struct {
		Status  int
		Payload map[string]interface{}
	}
	return ir.Payload, json.Unmarshal(b, &ir)
}

// https://github.com/Azure/azure-iot-sdk-node/blob/master/service/src/registry.ts
// https://docs.microsoft.com/en-us/azure/iot-hub/iot-hub-devguide-device-twins
type Twin struct {
	DeviceID                  string         `json:"deviceId"`
	ETag                      string         `json:"etag"`
	DeviceETag                string         `json:"deviceEtag"`
	Status                    string         `json:"status"`
	StatusReason              string         `json:"statusReason"`
	StatusUpdateTime          string         `json:"statusUpdateTime"`
	ConnectionState           string         `json:"connectionState"`
	LastActivityTime          string         `json:"lastActivityTime"`
	CloudToDeviceMessageCount int            `json:"cloudToDeviceMessageCount"`
	AuthenticationType        string         `json:"authenticationType"`
	X509Thumbprint            X509Thumbprint `json:"x509Thumbprint"`
	Version                   int            `json:"version"`
	// TODO: "tags": {
	//        "$etag": "123",
	//        "deploymentLocation": {
	//            "building": "43",
	//            "floor": "1"
	//        }
	//    },
	Properties   Properties             `json:"properties"`
	Capabilities map[string]interface{} `json:"capabilities"`

	RawJSON []byte `json:"-"`
}

type Properties struct {
	Desired  map[string]interface{} `json:"desired,omitempty"`
	Reported map[string]interface{} `json:"reported,omitempty"`
}

type X509Thumbprint struct {
	PrimaryThumbprint   string `json:"primaryThumbprint"`
	SecondaryThumbprint string `json:"secondaryThumbprint"`
}

type SymmetricKey struct {
	PrimaryKey   string `json:"primaryKey"`
	SecondaryKey string `json:"secondaryKey"`
}

type Device struct {
	DeviceID                   string `json:"deviceId"`
	GenerationID               string `json:"generationId"`
	ETag                       string `json:"etag"`
	ConnectionState            string `json:"connectionState"`
	Status                     string `json:"status"`
	StatusReason               string `json:"statusReason"`
	ConnectionStateUpdatedTime string `json:"connectionStateUpdatedTime"`
	StatusUpdatedTime          string `json:"statusUpdatedTime"`
	LastActivityTime           string `json:"lastActivityTime"`
	CloudToDeviceMessageCount  int    `json:"cloudToDeviceMessageCount"`
	Authentication             struct {
		X509Thumbprint X509Thumbprint `json:"x509Thumbprint"`
		Type           string         `json:"type"`
	} `json:"authentication"`
	Capabilities map[string]interface{} `json:"capabilities"`

	RawJSON []byte `json:"-"`
}

func (c *Client) UpdateTwin(
	ctx context.Context,
	deviceID string,
	desired map[string]interface{},
) (*Twin, error) {
	b, err := json.Marshal(&Twin{
		Properties: Properties{
			Desired: desired,
		},
	})
	if err != nil {
		return nil, err
	}
	b, err = c.request(ctx, http.MethodPatch, "twins/%s", deviceID, b)
	if err != nil {
		return nil, err
	}
	res := &Twin{RawJSON: b}
	if err := json.Unmarshal(b, res); err != nil {
		return nil, err
	}
	return res, nil
}

// TODO:
//   createDevice
//   updateDevice
//   listDevices
//   deleteDevice
//   add/delete/update devices (bulk)
//   import/export devices from/to blob
//   listJobs
//   getJob
//   cancelJob
//   getTwin
//   updateTwin
//   registryStats
func (c *Client) GetDevice(ctx context.Context, deviceID string) (*Device, error) {
	b, err := c.request(ctx, http.MethodGet, "devices/%s", deviceID, nil)
	if err != nil {
		return nil, err
	}
	d := &Device{RawJSON: b}
	if json.Unmarshal(b, d); err != nil {
		return nil, err
	}
	return d, nil
}

func (c *Client) request(ctx context.Context, method, path, deviceID string, b []byte) ([]byte, error) {
	r, err := http.NewRequest(method,
		fmt.Sprintf("https://%s/"+path+"?api-version=%s",
			c.creds.HostName, url.PathEscape(deviceID), common.APIVersion),
		bytes.NewReader(b),
	)
	if err != nil {
		return nil, err
	}

	// TODO: cache sas
	sas, err := c.creds.SAS(c.creds.HostName, time.Hour)
	if err != nil {
		return nil, err
	}
	rid, err := eventhub.RandString()
	if err != nil {
		return nil, err
	}

	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	r.Header.Set("Authorization", sas)
	r.Header.Set("Request-Id", rid)
	r.WithContext(ctx)

	res, err := c.http.Do(r)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	b, err = ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("code = %d, body = %q", res.StatusCode, string(b))
	}
	return b, nil
}

func (c *Client) logf(format string, v ...interface{}) {
	if c.logger != nil {
		c.logger.Printf(format, v...)
	}
}

func (c *Client) debugf(format string, v ...interface{}) {
	if c.debug {
		c.logf(format, v...)
	}
}

// Close closes transport.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return nil
	default:
		close(c.done)
	}
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}