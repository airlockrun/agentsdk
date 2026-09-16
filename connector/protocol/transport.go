package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const HostTransportProtocol = "airlock.host.v2"
const MaxHostRequests = 16
const MaxHostConnectorClaims = 32
const MaxHostMessageBytes = MaxChildFrameBytes + (64 << 10)

type HostHeartbeat struct {
	AccessMode               RemoteAccessMode `json:"accessMode"`
	ActiveManagementAttempts []ActiveAttempt  `json:"activeManagementAttempts"`
	ActiveConnectorAttempts  []ActiveAttempt  `json:"activeConnectorAttempts"`
}

// Each demand permits at most one claim. Only one demand may be outstanding.
type HostDemand struct {
	ConnectorCapacity int `json:"connectorCapacity"`
}

type HostConnectorEvent struct {
	ConnectorID string   `json:"connectorId"`
	JobID       string   `json:"jobId"`
	Event       JobEvent `json:"event"`
}

type HostConnectorCompletion struct {
	ConnectorID string        `json:"connectorId"`
	JobID       string        `json:"jobId"`
	Completion  JobCompletion `json:"completion"`
}

type HostManagementProgress struct {
	JobID string              `json:"jobId"`
	Event HostManagementEvent `json:"event"`
}

// HostMessage has exactly one typed payload. Replies reuse the request ID;
// an ack means the service committed the operation, not merely received it.
type HostMessage struct {
	Protocol             string                                  `json:"protocol"`
	ID                   string                                  `json:"id"`
	Sync                 *HostSyncRequest                        `json:"sync,omitempty"`
	Synced               *HostSyncResponse                       `json:"synced,omitempty"`
	Heartbeat            *HostHeartbeat                          `json:"heartbeat,omitempty"`
	Demand               *HostDemand                             `json:"demand,omitempty"`
	Work                 *HostWork                               `json:"work,omitempty"`
	Inventory            *HostConnectorInventoryMutationRequest  `json:"inventory,omitempty"`
	Inventoried          *HostConnectorInventoryMutationResponse `json:"inventoried,omitempty"`
	ConnectorEvent       *HostConnectorEvent                     `json:"connectorEvent,omitempty"`
	ConnectorCompletion  *HostConnectorCompletion                `json:"connectorCompletion,omitempty"`
	ManagementEvent      *HostManagementProgress                 `json:"managementEvent,omitempty"`
	ManagementCompletion *HostManagementCompletion               `json:"managementCompletion,omitempty"`
	Ack                  *struct{}                               `json:"ack,omitempty"`
	Error                *HostMessageError                       `json:"error,omitempty"`
}

type HostMessageError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (m HostMessage) Validate() error {
	if m.Protocol != HostTransportProtocol || m.ID == "" || len(m.ID) > 64 {
		return errors.New("connector protocol: invalid host transport version or correlation ID")
	}
	count := 0
	for _, present := range []bool{m.Sync != nil, m.Synced != nil, m.Heartbeat != nil, m.Demand != nil, m.Work != nil, m.Inventory != nil, m.Inventoried != nil, m.ConnectorEvent != nil, m.ConnectorCompletion != nil, m.ManagementEvent != nil, m.ManagementCompletion != nil, m.Ack != nil, m.Error != nil} {
		if present {
			count++
		}
	}
	if count != 1 {
		return errors.New("connector protocol: host message requires exactly one payload")
	}
	if m.Demand != nil && (m.Demand.ConnectorCapacity < 0 || m.Demand.ConnectorCapacity > MaxActiveAttempts) {
		return errors.New("connector protocol: invalid host capacity")
	}
	if m.Heartbeat != nil && (len(m.Heartbeat.ActiveManagementAttempts) > MaxActiveAttempts || len(m.Heartbeat.ActiveConnectorAttempts) > MaxActiveAttempts) {
		return errors.New("connector protocol: too many active attempts")
	}
	if m.Work != nil {
		work := m.Work
		count := 0
		for _, present := range []bool{work.ConnectorJob != nil, work.ManagementJob != nil, work.Cancel != nil} {
			if present {
				count++
			}
		}
		if count != 1 {
			return errors.New("connector protocol: work requires exactly one payload")
		}
		switch work.Kind {
		case HostWorkConnectorJob:
			if work.ConnectorJob == nil || work.ConnectorID == "" {
				return errors.New("connector protocol: invalid connector work")
			}
		case HostWorkConnectorCancel:
			if work.Cancel == nil || work.ConnectorID == "" {
				return errors.New("connector protocol: invalid cancellation")
			}
		case HostWorkShell, HostWorkConnectorInstall, HostWorkConnectorUpdate, HostWorkConnectorRemove, HostWorkConnectorRollback:
			if work.ManagementJob == nil {
				return errors.New("connector protocol: invalid management work")
			}
		default:
			return errors.New("connector protocol: unknown work kind")
		}
	}
	return nil
}

func DecodeHostMessage(data []byte) (HostMessage, error) {
	var message HostMessage
	if len(data) > MaxHostMessageBytes {
		return message, errors.New("connector protocol: host message too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&message); err != nil {
		return message, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return message, errors.New("connector protocol: trailing host message data")
	}
	return message, message.Validate()
}
