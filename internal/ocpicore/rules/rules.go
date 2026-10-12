package rules

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
)

// Owner is the identity advertised by the CPO during the Credentials handshake.
type Owner struct {
	CountryCode string
	PartyID     string
}

var ocpiDateTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z?$`)

// ValidateDateTime enforces the OCPI 2.2.1 DateTime limitations from section 16.2.
func ValidateDateTime(value string) error {
	if !ocpiDateTime.MatchString(value) {
		return fmt.Errorf("must be RFC 3339 UTC using Z or no timezone designator")
	}
	parseValue := value
	if !strings.HasSuffix(parseValue, "Z") {
		parseValue += "Z"
	}
	if _, err := time.Parse(time.RFC3339Nano, parseValue); err != nil {
		return fmt.Errorf("invalid DateTime: %w", err)
	}
	return nil
}

func Validate(module string, object map[string]any, owner Owner) []ocpi.Finding {
	var findings []ocpi.Finding
	switch module {
	case "locations":
		findings = append(findings, required(object, "§8.3.1", "country_code", "party_id", "id", "publish", "address", "city", "country", "coordinates", "time_zone", "last_updated")...)
		findings = append(findings, validateLocations(object)...)
	case "tariffs":
		findings = append(findings, required(object, "§11.3.1", "country_code", "party_id", "id", "currency", "elements", "last_updated")...)
	case "sessions":
		findings = append(findings, required(object, "§9.3.1", "country_code", "party_id", "id", "start_date_time", "kwh", "cdr_token", "auth_method", "location_id", "evse_uid", "connector_id", "currency", "status", "last_updated")...)
		if status := strings.ToUpper(text(object["status"])); status != "" && !oneOf(status, "ACTIVE", "COMPLETED", "INVALID", "PENDING", "RESERVATION") {
			findings = append(findings, errFinding("§9.4.1", "invalid Session.status: "+status))
		}
	case "cdrs":
		findings = append(findings, required(object, "§10.3.1", "country_code", "party_id", "id", "start_date_time", "end_date_time", "cdr_token", "auth_method", "cdr_location", "currency", "charging_periods", "total_cost", "total_energy", "total_time", "last_updated")...)
	case "tokens":
		findings = append(findings, required(object, "§12.3.2", "country_code", "party_id", "uid", "type", "contract_id", "issuer", "valid", "whitelist", "last_updated")...)
		if tokenType := strings.ToUpper(text(object["type"])); tokenType != "" && !oneOf(tokenType, "AD_HOC_USER", "APP_USER", "OTHER", "RFID") {
			findings = append(findings, errFinding("§12.4.2", "invalid Token.type: "+tokenType))
		}
		if whitelist := strings.ToUpper(text(object["whitelist"])); whitelist != "" && !oneOf(whitelist, "ALWAYS", "ALLOWED", "ALLOWED_OFFLINE", "NEVER") {
			findings = append(findings, errFinding("§12.4.3", "invalid Token.whitelist: "+whitelist))
		}
	default:
		return []ocpi.Finding{errFinding("scope", "unsupported module: "+module)}
	}
	if module != "cdrs" || object["country_code"] != nil {
		if cc := text(object["country_code"]); cc != "" && !strings.EqualFold(cc, owner.CountryCode) {
			findings = append(findings, errFinding("§4.1.5", fmt.Sprintf("country_code %q does not match CPO role %q", cc, owner.CountryCode)))
		}
		if pid := text(object["party_id"]); pid != "" && !strings.EqualFold(pid, owner.PartyID) {
			findings = append(findings, errFinding("§4.1.5", fmt.Sprintf("party_id %q does not match CPO role %q", pid, owner.PartyID)))
		}
	}
	findings = append(findings, validateTimes(object, "")...)
	return findings
}

func validateLocations(location map[string]any) []ocpi.Finding {
	var findings []ocpi.Finding
	evses, ok := asSlice(location["evses"])
	if !ok {
		return findings
	}
	for index, raw := range evses {
		evse, ok := raw.(map[string]any)
		if !ok {
			findings = append(findings, errFinding("§8.3.2", fmt.Sprintf("evses[%d] must be an object", index)))
			continue
		}
		findings = append(findings, required(evse, "§8.3.2", "uid", "status", "connectors", "last_updated")...)
		connectors, _ := asSlice(evse["connectors"])
		if len(connectors) == 0 {
			findings = append(findings, errFinding("§8.3.2", fmt.Sprintf("evses[%d].connectors must contain at least one Connector", index)))
		}
		for connectorIndex, connectorRaw := range connectors {
			connector, ok := connectorRaw.(map[string]any)
			if !ok {
				findings = append(findings, errFinding("§8.3.3", fmt.Sprintf("evses[%d].connectors[%d] must be an object", index, connectorIndex)))
				continue
			}
			findings = append(findings, required(connector, "§8.3.3", "id", "standard", "format", "power_type", "max_voltage", "max_amperage", "last_updated")...)
		}
	}
	return findings
}

func validateTimes(value any, path string) []ocpi.Finding {
	var findings []ocpi.Finding
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			if key == "last_updated" || strings.HasSuffix(key, "_date_time") || key == "start_date_time" || key == "end_date_time" {
				if textValue, ok := child.(string); ok {
					if err := ValidateDateTime(textValue); err != nil {
						findings = append(findings, errFinding("§16.2", childPath+": "+err.Error()))
					}
				}
			}
			findings = append(findings, validateTimes(child, childPath)...)
		}
	case []any:
		for index, child := range current {
			findings = append(findings, validateTimes(child, fmt.Sprintf("%s[%d]", path, index))...)
		}
	}
	return findings
}

func required(object map[string]any, section string, fields ...string) []ocpi.Finding {
	var findings []ocpi.Finding
	for _, field := range fields {
		value, ok := object[field]
		if !ok || value == nil || (fmt.Sprint(value) == "") {
			findings = append(findings, errFinding(section, "missing required field: "+field))
		}
	}
	return findings
}

func errFinding(section, message string) ocpi.Finding {
	return ocpi.Finding{Level: "error", Rule: "OCPI 2.2.1 " + section, Message: message}
}

func text(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func asSlice(value any) ([]any, bool) {
	items, ok := value.([]any)
	return items, ok
}

func oneOf(value string, allowed ...string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}
