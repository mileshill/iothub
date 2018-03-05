package commonamqp

import (
	"fmt"
	"time"

	"github.com/amenzhinsky/golang-iothub/common"
	"pack.ag/amqp"
)

func FromAMQPMessage(msg *amqp.Message) (*common.Message, error) {
	m := &common.Message{
		Payload:    msg.Data[0],
		Properties: make(map[string]string, len(msg.ApplicationProperties)+5),
	}
	if msg.Properties != nil {
		m.UserID = string(msg.Properties.UserID)
		m.MessageID = msg.Properties.MessageID.(string)
		m.CorrelationID = msg.Properties.CorrelationID.(string)
		m.To = msg.Properties.To
		m.ExpiryTime = msg.Properties.AbsoluteExpiryTime
	}
	for k, v := range msg.Annotations {
		switch k {
		case "iothub-enqueuedtime":
			m.EnqueuedTime = v.(time.Time)
		case "iothub-connection-device-id":
			m.ConnectionDeviceID = v.(string)
		case "iothub-connection-auth-generation-id":
			m.ConnectionDeviceGenerationID = v.(string)
		case "iothub-connection-auth-method":
			m.ConnectionAuthMethod = v.(string)
		case "iothub-message-source":
			m.MessageSource = v.(string)
		default:
			m.Properties[k.(string)] = fmt.Sprint(v)
		}
	}
	for k, v := range msg.ApplicationProperties {
		m.Properties[k] = v.(string)
	}
	return m, nil
}

func ToAMQPMessage(msg *common.Message) *amqp.Message {
	props := make(map[string]interface{}, len(msg.Properties))
	for k, v := range msg.Properties {
		props[k] = v
	}
	return &amqp.Message{
		Data: [][]byte{msg.Payload},
		Properties: &amqp.MessageProperties{
			To:                 msg.To,
			UserID:             []byte(msg.UserID),
			MessageID:          msg.MessageID,
			CorrelationID:      msg.CorrelationID,
			AbsoluteExpiryTime: msg.ExpiryTime,
		},
		ApplicationProperties: props,
	}
}
