package rbm

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nyaruka/courier"
	. "github.com/nyaruka/courier/handlers"
)

var testChannels = []courier.Channel{
	courier.NewMockChannel(
		"8eb23e93-5ecb-45ba-b726-3b064e0c568c",
		"RBM",
		"250788383383",
		"RW",
		map[string]interface{}{
			"auth_token": "the-auth-token",
			"send_url":   "https://foo.bar/",
		}),
}

var helloMsg = `{	
	"senderPhoneNumber": "+12223334444",
	"messageId": "msg000999888777a",
	"sendTime": "2018-12-31T15:01:23.045123456Z",
	"text": "hello world"
}`

var invalidFrom = `{
	"senderPhoneNumber": "not a number",
	"messageId": "msg000999888777a",
	"sendTime": "2018-12-31T15:01:23.045123456Z",
	"text": "hello world"
}`

var invalidTimestamp = `{
	"senderPhoneNumber": "+12223334444",
	"messageId": "msg000999888777a",
	"sendTime": "20170623T123000Z",
	"text": "hello world"
}`

var invalidMsg = `not json`

var testCases = []ChannelHandleTestCase{
	{Label: "Receive Valid Message", URL: "/c/rbm/8eb23e93-5ecb-45ba-b726-3b064e0c568c/receive", Data: helloMsg, Status: 200, Response: `"type":"msg"`,
		Text: Sp("hello world"), URN: Sp("rbm:+12223334444"), ExternalID: Sp("msg000999888777a"), Date: Tp(time.Date(2018, 12, 31, 15, 01, 23, 45123456, time.UTC))},
	{Label: "Receive Invalid JSON", URL: "/c/rbm/8eb23e93-5ecb-45ba-b726-3b064e0c568c/receive", Data: invalidMsg, Status: 400, Response: "unable to parse"},
	{Label: "Receive Invalid From", URL: "/c/rbm/8eb23e93-5ecb-45ba-b726-3b064e0c568c/receive", Data: invalidFrom, Status: 400, Response: "invalid rbm number"},
	{Label: "Receive Invalid Timestamp", URL: "/c/rbm/8eb23e93-5ecb-45ba-b726-3b064e0c568c/receive", Data: invalidTimestamp, Status: 400, Response: "invalid send time format"},
	{Label: "Receive Invalid JSON", URL: "/c/rbm/8eb23e93-5ecb-45ba-b726-3b064e0c568c/receive", Data: "not json", Status: 400, Response: "unable to parse"},
}

func TestHandler(t *testing.T) {
	RunChannelTestCases(t, testChannels, newHandler(), testCases)
}

func BenchmarkHandler(b *testing.B) {
	RunChannelBenchmarks(b, testChannels, newHandler(), testCases)
}

// setSendURL takes care of setting the base_url to our test server host
func setSendURL(s *httptest.Server, h courier.ChannelHandler, c courier.Channel, m courier.Msg) {
	c.(*courier.MockChannel).SetConfig("send_url", s.URL)
}

var defaultSendTestCases = []ChannelSendTestCase{
	{Label: "Plain Send",
		Text: "Simple Message", URN: "rbm:+250788123123",
		Status: "W", ExternalID: "157b5e14568e8",
		ResponseBody: `{ "name": "phones/+250788123123/agentMessages/157b5e14568e8" }`, ResponseStatus: 201,
		RequestBody: `{"contentMessage":{"text":"Simple Message"}}`,
		SendPrep:    setSendURL},
	{Label: "Unicode Send",
		Text: "☺", URN: "rbm:+250788123123",
		Status: "W", ExternalID: "157b5e14568e8",
		ResponseBody: `{ "name": "phones/+250788123123/agentMessages/157b5e14568e8" }`, ResponseStatus: 201,
		RequestBody: `{"contentMessage":{"text":"☺"}}`,
		SendPrep:    setSendURL},
	{Label: "Error",
		Text: "Error", URN: "rbm:+250788123123",
		Status:       "E",
		ResponseBody: `{ "error": { "status": "PERMISSION_DENIED" } }`, ResponseStatus: 403,
		RequestBody: `{"contentMessage":{"text":"Error"}}`,
		SendPrep:    setSendURL},
	{Label: "No Message ID",
		Text: "Error", URN: "rbm:+250788123123",
		Status:       "E",
		ResponseBody: `{ "name": "/" }`, ResponseStatus: 200,
		RequestBody: `{"contentMessage":{"text":"Error"}}`,
		SendPrep:    setSendURL},
}

func TestSending(t *testing.T) {
	var defaultChannel = courier.NewMockChannel("8eb23e93-5ecb-45ba-b726-3b064e0c56ab", "RBM", "250788383383", "US",
		map[string]interface{}{
			"auth_token": "token123",
			"base_url":   "https://foo.bar/",
		})

	RunChannelSendTestCases(t, defaultChannel, newHandler(), defaultSendTestCases, nil)
}
