package rbm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/buger/jsonparser"
	"github.com/nyaruka/courier"
	"github.com/nyaruka/courier/handlers"
	"github.com/nyaruka/courier/utils"
	"github.com/nyaruka/gocommon/urns"
	"github.com/pkg/errors"
	uuid "github.com/satori/go.uuid"
)

func init() {
	courier.RegisterHandler(newHandler())
}

type handler struct {
	handlers.BaseHandler
}

func newHandler() courier.ChannelHandler {
	return &handler{handlers.NewBaseHandler(courier.ChannelType("RBM"), "RBM")}
}

// Initialize is called by the engine once everything is loaded
func (h *handler) Initialize(s courier.Server) error {
	h.SetServer(s)
	s.AddHandlerRoute(h, http.MethodPost, "receive", h.receiveEvent)
	return nil
}

// {
// 	"senderPhoneNumber": "+12223334444",
// 	"messageId": "msg000999888777a",
// 	"sendTime": "2018-12-31T15:01:23.045123456Z",
// 	"text": "Hello to you too!",
// }
type eventPayload struct {
	SenderPhoneNumber string `json:"senderPhoneNumber" validate:"required"`
	MessageID         string `json:"messageId" validate:"required"`
	SendTime          string `json:"sendTime" validate:"required"`
	Text              string `json:"text"`
}

// receiveMessage is our HTTP handler function for incoming messages
func (h *handler) receiveEvent(ctx context.Context, channel courier.Channel, w http.ResponseWriter, r *http.Request) ([]courier.Event, error) {
	payload := &eventPayload{}
	err := handlers.DecodeAndValidateJSON(payload, r)
	if err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, err)
	}

	// the list of events we deal with
	events := make([]courier.Event, 0, 2)

	// the list of data we will return in our response
	data := make([]interface{}, 0, 2)

	// create our date from the timestamp
	date, err := time.Parse(time.RFC3339Nano, payload.SendTime)
	if err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, fmt.Errorf("invalid send time format, must be RFC 3339"))
	}

	// create our URN
	urn, err := urns.NewURNFromParts("rbm", payload.SenderPhoneNumber, "", "")
	fmt.Println("URN!!")
	fmt.Println(urn)
	if err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, err)
	}

	text := ""

	if payload.Text != "" {
		text = payload.Text
	} else {
		// we received a message type we do not support.
		courier.LogRequestError(r, channel, fmt.Errorf("unsupported message type %s", payload))
	}

	// create our message
	event := h.Backend().NewIncomingMsg(channel, urn, text).WithReceivedOn(date).WithExternalID(payload.MessageID)

	// we had an error downloading media
	if err != nil {
		courier.LogRequestError(r, channel, err)
	}

	err = h.Backend().WriteMsg(ctx, event)
	if err != nil {
		return nil, err
	}

	events = append(events, event)
	data = append(data, courier.NewMsgReceiveData(event))

	return events, courier.WriteDataResponse(ctx, w, http.StatusOK, "Events Handled", data)
}

type mtTextPayload struct {
	ContentMessage struct {
		Text string `json:"text" validate:"required"`
	} `json:"contentMessage"    validate:"required"`
}

// whatsapp only allows messages up to 4096 chars
const maxMsgLength = 4096

// SendMsg sends the passed in message, returning any error
func (h *handler) SendMsg(ctx context.Context, msg courier.Msg) (courier.MsgStatus, error) {
	// get our token
	token := msg.Channel().StringConfigForKey(courier.ConfigAuthToken, "")
	if token == "" {
		return nil, fmt.Errorf("missing token for RBM channel")
	}
	urlStr := msg.Channel().StringConfigForKey(courier.ConfigSendURL, "")
	if urlStr == "" {
		return nil, fmt.Errorf("missing send url for RBM channel")
	}
	url, err := url.Parse(urlStr)
	if err != nil {
		return nil, fmt.Errorf("invalid base url set for RBM channel: %s", err)
	}
	externalID := uuid.NewV4().String()
	url, _ = url.Parse(fmt.Sprintf("%s/phones/%s/agentMessages?messageId=%s", urlStr, msg.URN().Path(), externalID))
	sendURL := url.String()
	status := h.Backend().NewMsgStatusForID(msg.Channel(), msg.ID(), courier.MsgErrored)
	var log *courier.ChannelLog

	parts := handlers.SplitMsg(msg.Text(), maxMsgLength)
	for i, part := range parts {
		payload := mtTextPayload{}
		payload.ContentMessage.Text = part
		externalID, log, err = sendRbmMessage(msg, sendURL, token, payload)
		status.AddLog(log)
		if err != nil {
			log.WithError("Error sending message", err)
			break
		}
		// if this is our first message, record the external id
		if i == 0 {
			status.SetExternalID(externalID)
		}
	}

	// we are wired it there were no errors
	if err == nil {
		status.SetStatus(courier.MsgWired)
	}

	return status, nil
}

func uploadMediaToWhatsApp(msg courier.Msg, url string, token string, attachmentMimeType string, attachmentURL string) (string, *courier.ChannelLog, error) {
	// retrieve the media to be sent from S3
	req, _ := http.NewRequest(http.MethodGet, attachmentURL, nil)
	s3rr, err := utils.MakeHTTPRequest(req)
	if err != nil {
		return "", courier.NewChannelLogFromRR("Media Fetch", msg.Channel(), msg.ID(), s3rr), err
	}

	// upload it to WhatsApp in exchange for a media id
	rbmReq, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(s3rr.Body))
	rbmReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	rbmReq.Header.Set("Content-Type", attachmentMimeType)
	rbmReq.Header.Set("User-Agent", utils.HTTPUserAgent)
	rbmRr, err := utils.MakeHTTPRequest(rbmReq)

	log := courier.NewChannelLogFromRR("Media Upload success", msg.Channel(), msg.ID(), rbmRr)

	if err != nil {
		return "", log, err
	}

	mediaID, err := jsonparser.GetString(rbmRr.Body, "media", "[0]", "id")
	if err != nil {
		return "", log, err
	}

	return mediaID, log, nil
}

func sendRbmMessage(msg courier.Msg, url string, token string, payload interface{}) (string, *courier.ChannelLog, error) {
	jsonBody, err := json.Marshal(payload)
	if err != nil {
		log := courier.NewChannelLog("unable to build JSON body", msg.Channel(), msg.ID(), "", "", courier.NilStatusCode, "", "", time.Duration(0), err)
		return "", log, err
	}

	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Set("User-Agent", utils.HTTPUserAgent)
	rr, err := utils.MakeHTTPRequest(req)

	log := courier.NewChannelLogFromRR("Message Sent", msg.Channel(), msg.ID(), rr).WithError("Message Send Error", err)

	errorTitle, err := jsonparser.GetString(rr.Body, "error", "status")
	if errorTitle != "" {
		err = errors.Errorf("received error from send endpoint: %s", errorTitle)
		return "", log, err
	}

	// grab the id
	name, err := jsonparser.GetString(rr.Body, "name")
	if err != nil {
		err := errors.Errorf("unable to get message id from response body")
		return "", log, err
	}
	parts := strings.Split(name, "/")
	externalID := parts[len(parts)-1]
	if externalID == "" {
		err := errors.Errorf("external was an empty string")
		return "", log, err
	}

	return externalID, log, err
}
