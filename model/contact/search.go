package contact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
)

// ErrNoSearch is returned when the context of the instance has no
// autocomplete API, or the instance has no email to search for.
var ErrNoSearch = errors.New("no contacts search for this instance")

// The share modal calls the search as the user types, so a slow answer is
// dropped for the local search.
var searchClient = &http.Client{Timeout: 1 * time.Second}

// Person is a result of the autocomplete API of the calendar side service.
type Person struct {
	Name  string
	Email string
}

// Search asks the autocomplete API of the calendar side service for the people
// with an email that match q, in the address books of the owner of inst.
func Search(inst *instance.Instance, q string, limit int) ([]Person, error) {
	cfg, ok := config.GetContactsSearch(inst.ContextName)
	if !ok || inst.Email == "" {
		return nil, ErrNoSearch
	}
	body, err := json.Marshal(map[string]interface{}{
		"user":        inst.Email,
		"q":           q,
		"objectTypes": []string{"contact"},
		"hasFields":   []string{"emailAddresses"},
		"limit":       limit,
		"offset":      0,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	res, err := searchClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("contacts search: unexpected status %d", res.StatusCode)
	}

	var results []struct {
		Names []struct {
			DisplayName string `json:"displayName"`
		} `json:"names"`
		EmailAddresses []struct {
			Value string `json:"value"`
		} `json:"emailAddresses"`
	}
	if err := json.NewDecoder(res.Body).Decode(&results); err != nil {
		return nil, err
	}
	people := make([]Person, 0, len(results))
	for _, r := range results {
		// Someone without an email cannot be invited.
		if len(r.EmailAddresses) == 0 || r.EmailAddresses[0].Value == "" {
			continue
		}
		p := Person{Email: r.EmailAddresses[0].Value}
		if len(r.Names) > 0 {
			p.Name = r.Names[0].DisplayName
		}
		people = append(people, p)
	}
	return people, nil
}
