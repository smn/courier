package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/buger/jsonparser"
	"github.com/nyaruka/courier"
	"github.com/nyaruka/courier/handlers"
	"github.com/nyaruka/courier/utils"
	"github.com/nyaruka/gocommon/urns"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

func init() {
	courier.RegisterHandler(newHandler())
}

type handler struct {
	handlers.BaseHandler
}

func newHandler() courier.ChannelHandler {
	return &handler{handlers.NewBaseHandler(courier.ChannelType("WA"), "WhatsApp")}
}

// Initialize is called by the engine once everything is loaded
func (h *handler) Initialize(s courier.Server) error {
	h.SetServer(s)
	s.AddHandlerRoute(h, http.MethodPost, "receive", h.receiveEvent)
	return nil
}

// {
//   "statuses": [{
//     "id": "9712A34B4A8B6AD50F",
//     "recipient_id": "16315555555",
//     "status": "sent",
//     "timestamp": "1518694700"
//   }],
//   "messages": [ {
//     "from": "16315555555",
//     "id": "3AF99CB6BE490DCAF641",
//     "timestamp": "1518694235",
//     "text": {
//       "body": "Hello this is an answer"
//     },
//     "type": "text"
//   }]
// }
type eventPayload struct {
	Messages []struct {
		From      string `json:"from"      validate:"required"`
		ID        string `json:"id"        validate:"required"`
		Timestamp string `json:"timestamp" validate:"required"`
		Type      string `json:"type"      validate:"required"`
		Text      struct {
			Body string `json:"body"`
		} `json:"text"`
		Audio struct {
			File     string `json:"file"`
			ID       string `json:"id"`
			Link     string `json:"link"`
			MimeType string `json:"mime_type"`
			Sha256   string `json:"sha256"`
		} `json:"audio"`
		Document struct {
			File     string `json:"file"`
			ID       string `json:"id"`
			Link     string `json:"link"`
			MimeType string `json:"mime_type"`
			Sha256   string `json:"sha256"`
			Caption  string `json:"caption"`
		} `json:"document"`
		Image struct {
			File     string `json:"file"`
			ID       string `json:"id"`
			Link     string `json:"link"`
			MimeType string `json:"mime_type"`
			Sha256   string `json:"sha256"`
			Caption  string `json:"caption"`
		} `json:"image"`
		Location struct {
			Address   string  `json:"address"`
			Latitude  float32 `json:"latitude"`
			Longitude float32 `json:"longitude"`
			Name      string  `json:"name"`
			URL       string  `json:"url"`
		} `json:"location"`
		Video struct {
			File     string `json:"file"`
			ID       string `json:"id"`
			Link     string `json:"link"`
			MimeType string `json:"mime_type"`
			Sha256   string `json:"sha256"`
		} `json:"video"`
		Voice struct {
			File     string `json:"file"`
			ID       string `json:"id"`
			Link     string `json:"link"`
			MimeType string `json:"mime_type"`
			Sha256   string `json:"sha256"`
		} `json:"voice"`
	} `json:"messages"`
	Statuses []struct {
		ID          string `json:"id"           validate:"required"`
		RecipientID string `json:"recipient_id" validate:"required"`
		Timestamp   string `json:"timestamp"    validate:"required"`
		Status      string `json:"status"       validate:"required"`
	} `json:"statuses"`
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

	// first deal with any received messages
	for _, msg := range payload.Messages {

		// create our date from the timestamp
		ts, err := strconv.ParseInt(msg.Timestamp, 10, 64)
		if err != nil {
			return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, fmt.Errorf("invalid timestamp: %s", msg.Timestamp))
		}
		date := time.Unix(ts, 0).UTC()

		// create our URN
		urn, err := urns.NewWhatsAppURN(msg.From)
		if err != nil {
			return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, err)
		}

		text := ""
		mediaURL := ""

		if msg.Type == "text" {
			text = msg.Text.Body
		} else if msg.Type == "audio" {
			mediaURL, err = resolveMediaURL(channel, msg.Audio.ID)
		} else if msg.Type == "document" {
			text = msg.Document.Caption
			mediaURL, err = resolveMediaURL(channel, msg.Document.ID)
		} else if msg.Type == "image" {
			text = msg.Image.Caption
			mediaURL, err = resolveMediaURL(channel, msg.Image.ID)
		} else if msg.Type == "location" {
			mediaURL = fmt.Sprintf("geo:%f,%f", msg.Location.Latitude, msg.Location.Longitude)
		} else if msg.Type == "video" {
			mediaURL, err = resolveMediaURL(channel, msg.Video.ID)
		} else if msg.Type == "voice" {
			mediaURL, err = resolveMediaURL(channel, msg.Voice.ID)
		} else {
			// we received a message type we do not support.
			courier.LogRequestError(r, channel, fmt.Errorf("Unsupported message type %s", msg.Type))
		}

		// create our message
		event := h.Backend().NewIncomingMsg(channel, urn, text).WithReceivedOn(date).WithExternalID(msg.ID)

		// we had an error downloading media
		if err != nil {
			courier.LogRequestError(r, channel, err)
		}

		if mediaURL != "" {
			event.WithAttachment(mediaURL)
		}

		err = h.Backend().WriteMsg(ctx, event)
		if err != nil {
			return nil, err
		}

		events = append(events, event)
		data = append(data, courier.NewMsgReceiveData(event))
	}

	// now with any status updates
	for _, status := range payload.Statuses {
		msgStatus, found := waStatusMapping[status.Status]
		if !found {
			handlers.WriteAndLogRequestError(ctx, h, channel, w, r, fmt.Errorf("invalid status: %s", status.Status))
		}

		event := h.Backend().NewMsgStatusForExternalID(channel, status.ID, msgStatus)
		err := h.Backend().WriteMsgStatus(ctx, event)

		// we don't know about this message, just tell them we ignored it
		if err == courier.ErrMsgNotFound {
			data = append(data, courier.NewInfoData(fmt.Sprintf("message id: %s not found, ignored", status.ID)))
			continue
		}

		if err != nil {
			return nil, err
		}

		events = append(events, event)
		data = append(data, courier.NewStatusData(event))
	}

	return events, courier.WriteDataResponse(ctx, w, http.StatusOK, "Events Handled", data)
}

func resolveMediaURL(channel courier.Channel, mediaID string) (string, error) {
	token := channel.StringConfigForKey(courier.ConfigAuthToken, "")
	if token == "" {
		return "", fmt.Errorf("Missing token for WA channel")
	}

	urlStr := channel.StringConfigForKey(courier.ConfigBaseURL, "")
	url, err := url.Parse(urlStr)
	if err != nil {
		return "", fmt.Errorf("Invalid base url set for WA channel: %s", err)
	}

	mediaPath, _ := url.Parse("/v1/media")
	mediaEndpoint := url.ResolveReference(mediaPath).String()

	fileURL := fmt.Sprintf("%s/%s", mediaEndpoint, mediaID)

	return fileURL, nil
}

// BuildDownloadMediaRequest to download media for message attachment with Bearer token set
func (h *handler) BuildDownloadMediaRequest(ctx context.Context, b courier.Backend, channel courier.Channel, attachmentURL string) (*http.Request, error) {
	token := channel.StringConfigForKey(courier.ConfigAuthToken, "")
	if token == "" {
		return nil, fmt.Errorf("Missing token for WA channel")
	}

	logrus.WithField("build_download_media_request", token).WithField("attachmentURL", attachmentURL).Debug("S3 debugging")
	// set the access token as the authorization header
	req, _ := http.NewRequest(http.MethodGet, attachmentURL, nil)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	return req, nil
}

var waStatusMapping = map[string]courier.MsgStatusValue{
	"sending":   courier.MsgWired,
	"sent":      courier.MsgSent,
	"delivered": courier.MsgDelivered,
	"read":      courier.MsgDelivered,
	"failed":    courier.MsgFailed,
}

// {
//   "to": "16315555555",
//   "type": "text | audio | document | image",
//   "text": {
//     "body": "text message"
//   }
//	 "audio": {
//	   "id": "the-audio-id"
// 	 }
//	 "document": {
//	   "id": "the-document-id"
//     "caption": "the optional document caption"
// 	 }
//	 "image": {
//	   "id": "the-image-id"
//     "caption": "the optional image caption"
// 	 }
// }

type mtTextPayload struct {
	To   string `json:"to"    validate:"required"`
	Type string `json:"type"  validate:"required"`
	Text struct {
		Body string `json:"body" validate:"required"`
	} `json:"text"`
}

type mtAudioPayload struct {
	To    string `json:"to"    validate:"required"`
	Type  string `json:"type"  validate:"required"`
	Audio struct {
		ID string `json:"id" validate:"required"`
	} `json:"audio"`
}

type mtDocumentPayload struct {
	To       string `json:"to"    validate:"required"`
	Type     string `json:"type"  validate:"required"`
	Document struct {
		ID      string `json:"id" validate:"required"`
		Caption string `json:"caption,omitempty"`
	} `json:"document"`
}

type mtImagePayload struct {
	To    string `json:"to"    validate:"required"`
	Type  string `json:"type"  validate:"required"`
	Image struct {
		ID      string `json:"id" validate:"required"`
		Caption string `json:"caption,omitempty"`
	} `json:"image"`
}

// whatsapp only allows messages up to 4096 chars
const maxMsgLength = 4096

// SendMsg sends the passed in message, returning any error
func (h *handler) SendMsg(ctx context.Context, msg courier.Msg) (courier.MsgStatus, error) {
	start := time.Now()
	// get our token
	token := msg.Channel().StringConfigForKey(courier.ConfigAuthToken, "")
	if token == "" {
		return nil, fmt.Errorf("missing token for WA channel")
	}

	urlStr := msg.Channel().StringConfigForKey(courier.ConfigBaseURL, "")
	url, err := url.Parse(urlStr)
	if err != nil {
		return nil, fmt.Errorf("invalid base url set for WA channel: %s", err)
	}
	sendPath, _ := url.Parse("/v1/messages")
	sendURL := url.ResolveReference(sendPath).String()

	mediaPath, _ := url.Parse("/v1/media")
	mediaURL := url.ResolveReference(mediaPath).String()

	status := h.Backend().NewMsgStatusForID(msg.Channel(), msg.ID(), courier.MsgErrored)

	// TODO: figure out sending media
	if len(msg.Attachments()) > 1 {
		duration := time.Now().Sub(start)
		err = fmt.Errorf("Message has %d attachments", len(msg.Attachments()))
		courier.NewChannelLogFromError("WhatsApp only allows for a single attachment to a message.", msg.Channel(), msg.ID(), duration, err)
		return status, err

	} else if len(msg.Attachments()) == 1 {

		attachment := msg.Attachments()[0]
		parts := strings.SplitN(attachment, ":", 2)
		mimeType := parts[0]
		s3url := parts[1]

		// retrieve the media to be sent from S3
		req, _ := http.NewRequest(http.MethodGet, s3url, nil)
		s3rr, err := utils.MakeHTTPRequest(req)
		if err != nil {
			log := courier.NewChannelLogFromRR("Error downloading Media for sending", msg.Channel(), msg.ID(), s3rr).WithError("Message Send Error", err)
			status.AddLog(log)
			return status, err
		}

		// upload it to WhatsApp in exchange for a media id
		waReq, _ := http.NewRequest(http.MethodPost, mediaURL, bytes.NewReader(s3rr.Body))
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
		req.Header.Set("Content-Type", mimeType)
		wArr, err := utils.MakeHTTPRequest(waReq)
		if err != nil {
			log := courier.NewChannelLogFromRR("Error uploading Media for sending", msg.Channel(), msg.ID(), wArr).WithError("Message Send Error", err)
			status.AddLog(log)
			return status, err
		}
		fmt.Println(string(wArr.Body[:]))

		mediaID, err := jsonparser.GetString(wArr.Body, "media", "[0]", "id")
		if err != nil {
			log := courier.NewChannelLogFromRR("Unable to read Media ID from WhatsApp server response", msg.Channel(), msg.ID(), wArr).WithError("JSON error", err)
			status.AddLog(log)
			return status, err
		}

		externalID := ""
		if strings.HasPrefix(mimeType, "audio") {
			payload := mtAudioPayload{
				To:   msg.URN().Path(),
				Type: "audio",
			}
			payload.Audio.ID = mediaID
			externalID, err = sendWhatsAppMsg(sendURL, token, payload)

		} else if strings.HasPrefix(mimeType, "application") {
			payload := mtDocumentPayload{
				To:   msg.URN().Path(),
				Type: "document",
			}
			payload.Document.ID = mediaID
			payload.Document.Caption = msg.Text()
			externalID, err = sendWhatsAppMsg(sendURL, token, payload)

		} else if strings.HasPrefix(mimeType, "image") {
			payload := mtImagePayload{
				To:   msg.URN().Path(),
				Type: "image",
			}
			payload.Image.ID = mediaID
			payload.Image.Caption = msg.Text()
			externalID, err = sendWhatsAppMsg(sendURL, token, payload)

		} else {
			err = fmt.Errorf("Unknown attachment mime type: %s", mimeType)
		}

		if err != nil {
			// record our status and log
			duration := time.Now().Sub(start)
			log := courier.NewChannelLogFromError("Error sending message", msg.Channel(), msg.ID(), duration, err)
			status.AddLog(log)
			return status, err
		}

		status.SetExternalID(externalID)

	} else {
		parts := handlers.SplitMsg(msg.Text(), maxMsgLength)
		for i, part := range parts {
			payload := mtTextPayload{
				To:   msg.URN().Path(),
				Type: "text",
			}
			payload.Text.Body = part

			externalID, err := sendWhatsAppMsg(sendURL, token, payload)
			if err != nil {
				// record our status and log
				duration := time.Now().Sub(start)
				log := courier.NewChannelLogFromError("Error sending message", msg.Channel(), msg.ID(), duration, err)
				status.AddLog(log)
				return status, err
			}

			// if this is our first message, record the external id
			if i == 0 {
				status.SetExternalID(externalID)
			}
		}

	}

	status.SetStatus(courier.MsgWired)
	return status, nil
}

func sendWhatsAppMsg(url string, token string, payload interface{}) (string, error) {

	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	rr, err := utils.MakeHTTPRequest(req)

	errorTitle, err := jsonparser.GetString(rr.Body, "errors", "[0]", "title")
	if errorTitle != "" {
		err = errors.Errorf("Received error from send endpoint: %s", errorTitle)
		return "", err
	}

	// grab the id
	externalID, err := jsonparser.GetString(rr.Body, "messages", "[0]", "id")
	if err != nil {
		fmt.Println(err)
		err := errors.Errorf("Unable to get message id from response body")
		return "", err
	}

	return externalID, err
}
