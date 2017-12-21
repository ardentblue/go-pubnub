package pubnub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"

	"github.com/pubnub/go/pnerr"
)

var WHERE_NOW_PATH = "/v2/presence/sub-key/%s/uuid/%s"

var emptyWhereNowResponse *WhereNowResponse

type whereNowBuilder struct {
	opts *whereNowOpts
}

func newWhereNowBuilder(pubnub *PubNub) *whereNowBuilder {
	builder := whereNowBuilder{
		opts: &whereNowOpts{
			pubnub: pubnub,
		},
	}

	return &builder
}

func newWhereNowBuilderWithContext(pubnub *PubNub,
	context Context) *whereNowBuilder {
	builder := whereNowBuilder{
		opts: &whereNowOpts{
			pubnub: pubnub,
			ctx:    context,
		},
	}

	return &builder
}

func (b *whereNowBuilder) Uuid(uuid string) *whereNowBuilder {
	b.opts.Uuid = uuid

	return b
}

func (b *whereNowBuilder) Execute() (*WhereNowResponse, StatusResponse, error) {
	rawJson, status, err := executeRequest(b.opts)
	if err != nil {
		return emptyWhereNowResponse, status, err
	}

	return newWhereNowResponse(rawJson, status)
}

type whereNowOpts struct {
	pubnub *PubNub

	Uuid string

	Transport http.RoundTripper

	ctx Context
}

func (o *whereNowOpts) config() Config {
	return *o.pubnub.Config
}

func (o *whereNowOpts) client() *http.Client {
	return o.pubnub.GetClient()
}

func (o *whereNowOpts) context() Context {
	return o.ctx
}

func (o *whereNowOpts) validate() error {
	if o.config().SubscribeKey == "" {
		return newValidationError(o, StrMissingSubKey)
	}

	if o.Uuid == "" {
		return newValidationError(o, StrMissingUuid)
	}

	return nil
}

func (o *whereNowOpts) buildPath() (string, error) {
	return fmt.Sprintf(WHERE_NOW_PATH,
		o.pubnub.Config.SubscribeKey,
		o.Uuid), nil
}

func (o *whereNowOpts) buildQuery() (*url.Values, error) {
	q := defaultQuery(o.pubnub.Config.Uuid, o.pubnub.telemetryManager)

	return q, nil
}

func (o *whereNowOpts) buildBody() ([]byte, error) {
	return []byte{}, nil
}

func (o *whereNowOpts) httpMethod() string {
	return "GET"
}

func (o *whereNowOpts) isAuthRequired() bool {
	return true
}

func (o *whereNowOpts) requestTimeout() int {
	return o.pubnub.Config.NonSubscribeRequestTimeout
}

func (o *whereNowOpts) connectTimeout() int {
	return o.pubnub.Config.ConnectTimeout
}

func (o *whereNowOpts) operationType() OperationType {
	return PNWhereNowOperation
}

func (o *whereNowOpts) telemetryManager() *TelemetryManager {
	return o.pubnub.telemetryManager
}

type WhereNowResponse struct {
	Channels []string
}

func newWhereNowResponse(jsonBytes []byte, status StatusResponse) (
	*WhereNowResponse, StatusResponse, error) {
	resp := &WhereNowResponse{}

	var value interface{}

	err := json.Unmarshal(jsonBytes, &value)
	if err != nil {
		e := pnerr.NewResponseParsingError("Error unmarshalling response",
			ioutil.NopCloser(bytes.NewBufferString(string(jsonBytes))), err)

		return emptyWhereNowResponse, status, e
	}

	if parsedValue, ok := value.(map[string]interface{}); ok {
		if payload, ok := parsedValue["payload"].(map[string]interface{}); ok {
			if channels, ok := payload["channels"].([]interface{}); ok {
				for _, ch := range channels {
					if channel, ok := ch.(string); ok {
						resp.Channels = append(resp.Channels, channel)
					}
				}
			}
		}
	}

	return resp, status, nil
}
