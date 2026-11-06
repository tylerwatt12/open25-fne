package framework

type adminPlannedPage struct {
	Title, Kind string
	Fields      []webField
}

var adminPlannedPages = map[string]adminPlannedPage{
	"alarm-settings": {Title: "Alarm settings", Kind: "settings", Fields: []webField{
		{Name: "id", Label: "Alarm identity", Kind: "text", Choices: "", Help: ""},
		{Name: "name", Label: "Alarm name", Kind: "text", Choices: "", Help: ""},
		{Name: "enabled", Label: "Monitoring enabled", Kind: "bool", Choices: "", Help: ""},
		{Name: "element_type", Label: "Element type", Kind: "select", Choices: "service=Service|zone=Zone|site=Site|channel=RF channel|modem=USB modem|application=Application Server", Help: ""},
		{Name: "element_id", Label: "Element identity", Kind: "text", Choices: "", Help: ""},
		{Name: "category", Label: "Category", Kind: "select", Choices: "communication=Communication & Synchronization|equipment=Equipment|environmental=Environmental|service=Service", Help: ""},
		{Name: "severity", Label: "Assigned severity", Kind: "select", Choices: "critical=Critical|major=Major|minor=Minor|warning=Warning|info=Information|unknown=Unknown|comm_failure=Communication failure", Help: "Severity mappings are part of the planned alarm backend; these are design examples."},
		{Name: "raise_delay_seconds", Label: "Raise delay (seconds)", Kind: "number", Choices: "", Help: "Proposed timing field. Repeated observations belong to one alarm instance."},
		{Name: "recovery_delay_seconds", Label: "Recovery delay (seconds)", Kind: "number", Choices: "", Help: "Proposed timing field. Recovery and operator acknowledgment stay separate."},
	}},
	"notification-rules": {Title: "Notification rules", Kind: "settings", Fields: []webField{
		{Name: "id", Label: "Rule identity", Kind: "text", Choices: "", Help: ""},
		{Name: "name", Label: "Rule name", Kind: "text", Choices: "", Help: ""},
		{Name: "enabled", Label: "Rule enabled", Kind: "bool", Choices: "", Help: ""},
		{Name: "scope", Label: "Alarm scope", Kind: "select", Choices: "local=This service|descendants=This service and descendants", Help: ""},
		{Name: "alarm_ids", Label: "Named alarm identities", Kind: "", Choices: "", Help: "Optional named conditions. These identities are design placeholders for the planned backend."},
		{Name: "categories", Label: "Categories", Kind: "flags", Choices: "communication=Communication & Synchronization|equipment=Equipment|environmental=Environmental|service=Service", Help: ""},
		{Name: "severities", Label: "Severities", Kind: "flags", Choices: "critical=Critical|major=Major|minor=Minor|warning=Warning|info=Information|unknown=Unknown|comm_failure=Communication failure", Help: ""},
		{Name: "transitions", Label: "Send when", Kind: "flags", Choices: "active=Condition becomes active|acknowledged=Operator acknowledges|recovered=Condition returns to normal", Help: "A condition transition requests notification. Viewing or refreshing this page never sends email."},
	}},
	"smtp-settings": {Title: "SMTP settings", Kind: "settings", Fields: []webField{
		{Name: "inherit_smtp", Label: "SMTP source", Kind: "bool", Choices: "", Help: "Local fields remain editable and preserved. The parent sends inherited mail without sharing its password."},
		{Name: "enabled", Label: "Local profile enabled", Kind: "bool", Choices: "", Help: ""},
		{Name: "host", Label: "SMTP server", Kind: "text", Choices: "", Help: ""},
		{Name: "port", Label: "Port", Kind: "number", Choices: "", Help: ""},
		{Name: "authentication", Label: "Authentication", Kind: "select", Choices: "none=None|password=Username and password", Help: ""},
		{Name: "username", Label: "Username", Kind: "text", Choices: "", Help: ""},
		{Name: "smtp_password", Label: "Replace password", Kind: "password", Choices: "", Help: "Enter only to replace the local password. Stored passwords never return to the browser."},
		{Name: "sender_name", Label: "Sender name", Kind: "text", Choices: "", Help: ""},
		{Name: "sender_address", Label: "Sender address", Kind: "text", Choices: "", Help: ""},
		{Name: "recipients", Label: "Recipients", Kind: "", Choices: "", Help: "Addresses for this profile. Saving does not test or send email."},
	}},
	"alarm-actions": {Title: "Alarm acknowledgment", Kind: "planned-operation", Fields: []webField{
		{Name: "alarm_id", Label: "Alarm identity", Kind: "text", Choices: "", Help: ""},
	}},
	"notification-actions": {Title: "Email test and retry", Kind: "planned-operation", Fields: []webField{
		{Name: "operation", Label: "Operation", Kind: "select", Choices: "test=Send test using saved SMTP profile|retry=Retry a failed delivery", Help: ""},
		{Name: "delivery_id", Label: "Delivery identity", Kind: "", Choices: "", Help: "Used for an explicit delivery retry."},
	}},
}
