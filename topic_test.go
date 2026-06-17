package agentsdk

import (
	"context"
	"testing"
)

func TestPublish_PanicsOnPerUserTopic(t *testing.T) {
	a, _ := testAgent(t)
	h := a.RegisterTopic(&Topic{Slug: "reminders", PerUser: true})
	defer func() {
		if recover() == nil {
			t.Fatal("Publish (broadcast) on a PerUser topic should panic")
		}
	}()
	_ = h.Publish(context.Background(), []DisplayPart{{Type: "text", Text: "x"}})
}

func TestPublishToUser_RequiresUserID(t *testing.T) {
	a, _ := testAgent(t)
	h := a.RegisterTopic(&Topic{Slug: "alerts", PerUser: true})
	defer func() {
		if recover() == nil {
			t.Fatal("PublishToUser with empty userID should panic")
		}
	}()
	_ = h.PublishToUser(context.Background(), "", []DisplayPart{{Type: "text", Text: "x"}})
}
