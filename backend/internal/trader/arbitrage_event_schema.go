package trader

func withEventSchema(payload map[string]any) map[string]any {
	if payload == nil {
		payload = map[string]any{}
	}
	if _, exists := payload["schemaVersion"]; !exists {
		payload["schemaVersion"] = 1
	}
	return payload
}
