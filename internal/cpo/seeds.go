package cpo

import (
	"fmt"
	"time"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpicore/rules"
)

func loadSeeds(state *memoryState) {
	if state.Objects == nil {
		state.Objects = emptyObjects()
	}
	timestamp := time.Now().UTC().Format(time.RFC3339)
	owner := rules.Owner{CountryCode: state.Config.CountryCode, PartyID: state.Config.PartyID}
	tariffs := []map[string]any{
		{
			"country_code": owner.CountryCode, "party_id": owner.PartyID, "id": "BKK-ENERGY", "currency": "THB",
			"tariff_alt_text": []any{map[string]any{"language": "en", "text": "Bangkok energy tariff"}, map[string]any{"language": "th", "text": "อัตราค่าพลังงานกรุงเทพ"}},
			"elements":        []any{map[string]any{"price_components": []any{map[string]any{"type": "ENERGY", "price": 7.5, "vat": 7.0, "step_size": 1}}}},
			"last_updated":    timestamp,
		},
		{
			"country_code": owner.CountryCode, "party_id": owner.PartyID, "id": "BKK-MIXED", "currency": "THB",
			"tariff_alt_text": []any{map[string]any{"language": "en", "text": "Energy, start and parking tariff"}},
			"elements": []any{
				map[string]any{"price_components": []any{map[string]any{"type": "FLAT", "price": 10.0, "vat": 7.0, "step_size": 1}, map[string]any{"type": "ENERGY", "price": 8.0, "vat": 7.0, "step_size": 1}}},
				map[string]any{"price_components": []any{map[string]any{"type": "PARKING_TIME", "price": 30.0, "vat": 7.0, "step_size": 900}}, "restrictions": map[string]any{"start_time": "18:00", "end_time": "23:59", "day_of_week": []any{"MONDAY", "TUESDAY", "WEDNESDAY", "THURSDAY", "FRIDAY"}}},
				map[string]any{"price_components": []any{map[string]any{"type": "PARKING_TIME", "price": 30.0, "vat": 7.0, "step_size": 900}}},
			},
			"last_updated": timestamp,
		},
	}
	for _, tariff := range tariffs {
		id := fmt.Sprint(tariff["id"])
		state.Objects["tariffs"][id] = &Record{Body: tariff, Delivery: "NOT_SENT", Findings: rules.Validate("tariffs", tariff, owner), UpdatedAt: timestamp}
	}
	locations := []map[string]any{
		seedLocation(owner, timestamp, "BKK-SILOM", "Evolt Partner Silom", "1 Si Lom Road", "Bang Rak", "10500", "13.728600", "100.534000", "ON_STREET", true, "SIL", []string{"BKK-ENERGY"}),
		seedLocation(owner, timestamp, "BKK-ARI", "Evolt Partner Ari", "14 Phahonyothin Road", "Phaya Thai", "10400", "13.779900", "100.544100", "PARKING_LOT", false, "ARI", []string{"BKK-MIXED"}),
		seedLocation(owner, timestamp, "BKK-SATHORN", "Evolt Partner Sathorn", "98 North Sathon Road", "Bang Rak", "10500", "13.721900", "100.529500", "UNDERGROUND_GARAGE", true, "SAT", []string{"BKK-ENERGY", "BKK-MIXED"}),
	}
	for _, location := range locations {
		id := fmt.Sprint(location["id"])
		state.Objects["locations"][id] = &Record{Body: location, Delivery: "NOT_SENT", Findings: rules.Validate("locations", location, owner), UpdatedAt: timestamp}
	}
}

func seedLocation(owner rules.Owner, timestamp, id, name, address, city, postal, latitude, longitude, parking string, alwaysOpen bool, prefix string, tariffIDs []string) map[string]any {
	opening := map[string]any{"twentyfourseven": alwaysOpen}
	if !alwaysOpen {
		opening["regular_hours"] = []any{
			map[string]any{"weekday": 1, "period_begin": "07:00", "period_end": "22:00"},
			map[string]any{"weekday": 2, "period_begin": "07:00", "period_end": "22:00"},
			map[string]any{"weekday": 3, "period_begin": "07:00", "period_end": "22:00"},
			map[string]any{"weekday": 4, "period_begin": "07:00", "period_end": "22:00"},
			map[string]any{"weekday": 5, "period_begin": "07:00", "period_end": "22:00"},
		}
	}
	return map[string]any{
		"country_code": owner.CountryCode, "party_id": owner.PartyID, "id": id, "publish": true, "name": name,
		"address": address, "city": city, "postal_code": postal, "country": "THA",
		"coordinates": map[string]any{"latitude": latitude, "longitude": longitude}, "parking_type": parking,
		"operator": map[string]any{"name": "Evolt Partner CPO Mock", "website": "https://example.test"},
		"owner":    map[string]any{"name": "Evolt Partner CPO Mock"}, "facilities": []any{"RESTAURANT", "WIFI"},
		"time_zone": "Asia/Bangkok", "opening_times": opening, "charging_when_closed": true,
		"images": []any{map[string]any{"url": "https://images.unsplash.com/photo-1597404294360-feeeda04612e", "thumbnail": "https://images.unsplash.com/photo-1597404294360-feeeda04612e?w=320", "category": "LOCATION", "type": "jpeg"}},
		"evses": []any{
			map[string]any{"uid": prefix + "-EVSE-1", "evse_id": owner.CountryCode + "*" + owner.PartyID + "*E" + prefix + "001", "status": "AVAILABLE", "capabilities": []any{"REMOTE_START_STOP_CAPABLE", "RFID_READER"}, "physical_reference": prefix + "-01", "connectors": []any{
				map[string]any{"id": "1", "standard": "IEC_62196_T2", "format": "SOCKET", "power_type": "AC_3_PHASE", "max_voltage": 400, "max_amperage": 32, "max_electric_power": 22000, "tariff_ids": stringAny(tariffIDs), "last_updated": timestamp},
			}, "last_updated": timestamp},
			map[string]any{"uid": prefix + "-EVSE-2", "evse_id": owner.CountryCode + "*" + owner.PartyID + "*E" + prefix + "002", "status": "AVAILABLE", "capabilities": []any{"REMOTE_START_STOP_CAPABLE", "RFID_READER", "START_SESSION_CONNECTOR_REQUIRED"}, "physical_reference": prefix + "-02", "connectors": []any{
				map[string]any{"id": "1", "standard": "IEC_62196_T2_COMBO", "format": "CABLE", "power_type": "DC", "max_voltage": 920, "max_amperage": 200, "max_electric_power": 150000, "tariff_ids": stringAny(tariffIDs), "last_updated": timestamp},
				map[string]any{"id": "2", "standard": "CHADEMO", "format": "CABLE", "power_type": "DC", "max_voltage": 500, "max_amperage": 125, "max_electric_power": 50000, "tariff_ids": stringAny(tariffIDs), "last_updated": timestamp},
			}, "last_updated": timestamp},
		},
		"last_updated": timestamp,
	}
}

func stringAny(values []string) []any {
	result := make([]any, len(values))
	for index, value := range values {
		result[index] = value
	}
	return result
}
