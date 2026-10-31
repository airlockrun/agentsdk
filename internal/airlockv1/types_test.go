package airlockv1

import (
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestHostEventTaskIdentity(t *testing.T) {
	const taskID = "554ee3cf-829f-4474-a509-3b2a9c8e1044"
	for _, tc := range []struct {
		name   string
		taskID *string
	}{
		{name: "uncorrelated observation"},
		{name: "exact task correlation", taskID: proto.String(taskID)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := &HostEvent{
				Kind:    HostEventKind_HOST_EVENT_KIND_WORKSPACE_COMMAND,
				Status:  HostEventStatus_HOST_EVENT_STATUS_SUCCEEDED,
				Details: "Diagnostic text containing task:" + taskID,
				TaskId:  tc.taskID,
			}
			field := event.ProtoReflect().Descriptor().Fields().ByName("task_id")
			if field == nil || field.Number() != 4 || !field.HasPresence() {
				t.Fatal("task_id must be an optional typed correlation field")
			}
			data, err := protojson.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			restored := new(HostEvent)
			if err := protojson.Unmarshal(data, restored); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(event, restored) || (restored.TaskId == nil) != (tc.taskID == nil) {
				t.Fatalf("event correlation changed on JSON round trip: %s", data)
			}
		})
	}
}
