package stream

import (
	"encoding/json"
	"testing"
)

// TestOrderUpdatesSubscriptionFormat verifies the subscription message format
func TestOrderUpdatesSubscriptionFormat(t *testing.T) {
	testAddress := "0x1234567890abcdef1234567890abcdef12345678"

	sub := SubscriptionFilter{
		Type: "orderUpdates",
		User: testAddress,
	}

	cmd := WsCommand{
		Method:       "subscribe",
		Subscription: &sub,
	}

	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}

	expected := `{"method":"subscribe","subscription":{"type":"orderUpdates","user":"0x1234567890abcdef1234567890abcdef12345678"}}`
	actual := string(data)

	t.Logf("Expected: %s", expected)
	t.Logf("Actual:   %s", actual)

	// Parse both to compare structure
	var expectedMap, actualMap map[string]interface{}
	json.Unmarshal([]byte(expected), &expectedMap)
	json.Unmarshal(data, &actualMap)

	expectedJSON, _ := json.Marshal(expectedMap)
	actualJSON, _ := json.Marshal(actualMap)

	if string(expectedJSON) != string(actualJSON) {
		t.Errorf("Subscription format mismatch!\nExpected: %s\nGot:      %s", expectedJSON, actualJSON)
	} else {
		t.Log("subscription format is correct")
	}
}

// TestUserEventsSubscriptionFormat verifies the userEvents subscription format
func TestUserEventsSubscriptionFormat(t *testing.T) {
	testAddress := "0x1234567890abcdef1234567890abcdef12345678"

	sub := SubscriptionFilter{
		Type: "userEvents",
		User: testAddress,
	}

	cmd := WsCommand{
		Method:       "subscribe",
		Subscription: &sub,
	}

	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}

	expected := `{"method":"subscribe","subscription":{"type":"userEvents","user":"0x1234567890abcdef1234567890abcdef12345678"}}`
	actual := string(data)

	t.Logf("Expected: %s", expected)
	t.Logf("Actual:   %s", actual)

	// Parse both to compare structure
	var expectedMap, actualMap map[string]interface{}
	json.Unmarshal([]byte(expected), &expectedMap)
	json.Unmarshal(data, &actualMap)

	expectedJSON, _ := json.Marshal(expectedMap)
	actualJSON, _ := json.Marshal(actualMap)

	if string(expectedJSON) != string(actualJSON) {
		t.Errorf("Subscription format mismatch!\nExpected: %s\nGot:      %s", expectedJSON, actualJSON)
	} else {
		t.Log("subscription format is correct")
	}
}
