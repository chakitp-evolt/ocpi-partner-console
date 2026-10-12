package cpo

import (
	"strings"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpicore/rules"
)

func (c *Controller) rebaseDraftOwner(previous rules.Owner) {
	current := rules.Owner{CountryCode: c.state.Config.CountryCode, PartyID: c.state.Config.PartyID}
	if current == previous {
		return
	}
	timestamp := now()
	oldPrefix := previous.CountryCode + "*" + previous.PartyID + "*"
	newPrefix := current.CountryCode + "*" + current.PartyID + "*"
	for _, module := range []string{"locations", "tariffs"} {
		for _, record := range c.state.Objects[module] {
			if record.Delivery != "NOT_SENT" || fieldText(record.Body, "country_code") != previous.CountryCode || fieldText(record.Body, "party_id") != previous.PartyID {
				continue
			}
			record.Body["country_code"], record.Body["party_id"] = current.CountryCode, current.PartyID
			record.Body["last_updated"] = timestamp
			if module == "locations" {
				evses, _ := record.Body["evses"].([]any)
				for _, raw := range evses {
					evse, _ := raw.(map[string]any)
					if id := fieldText(evse, "evse_id"); strings.HasPrefix(id, oldPrefix) {
						evse["evse_id"] = newPrefix + strings.TrimPrefix(id, oldPrefix)
						evse["last_updated"] = timestamp
					}
				}
			}
			record.Findings = rules.Validate(module, record.Body, current)
			record.UpdatedAt = timestamp
		}
	}
}
