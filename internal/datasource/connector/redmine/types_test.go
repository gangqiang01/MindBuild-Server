package redmine

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedmineValueUnmarshal(t *testing.T) {
	cases := []struct {
		name string
		json string
		want redmineValue
	}{
		{name: "string", json: `"handheld"`, want: "handheld"},
		{name: "empty string", json: `""`, want: ""},
		{name: "null", json: `null`, want: ""},
		{name: "string array", json: `["iPhone","iPad"]`, want: "iPhone, iPad"},
		{name: "empty array", json: `[]`, want: ""},
		{name: "mixed array", json: `["iPhone",12,true,null]`, want: "iPhone, 12, true, "},
		{name: "number", json: `42`, want: "42"},
		{name: "bool", json: `true`, want: "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got redmineValue
			if err := json.Unmarshal([]byte(tc.json), &got); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.json, err)
			}
			if got != tc.want {
				t.Fatalf("unmarshal %s: got %q, want %q", tc.json, got, tc.want)
			}
		})
	}
}

// TestIssueDecodeMultiValueCustomFields reproduces the sync failure where a
// Redmine project uses multi-value custom fields: their "value" is a JSON
// array, which used to abort decoding of the whole /issues.json page with
// "json: cannot unmarshal array into Go struct field ... of type string".
func TestIssueDecodeMultiValueCustomFields(t *testing.T) {
	body := `{
	  "issues": [
	    {
	      "id": 100,
	      "subject": "device feedback",
	      "custom_fields": [
	        {"id": 1, "name": "Devices", "value": ["iPhone", "iPad"]},
	        {"id": 2, "name": "Note", "value": "handheld"},
	        {"id": 3, "name": "Empty", "value": null}
	      ],
	      "journals": [
	        {
	          "id": 9,
	          "details": [
	            {"property": "cf", "name": "Devices", "old_value": ["iPhone"], "new_value": ["iPhone", "iPad"]}
	          ]
	        }
	      ]
	    }
	  ],
	  "total_count": 1, "offset": 0, "limit": 100
	}`

	var resp issuesResp
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode issues with array custom field values: %v", err)
	}
	if len(resp.Issues) != 1 {
		t.Fatalf("expected 1 issue, got %d", len(resp.Issues))
	}
	iss := resp.Issues[0]
	wantFields := map[string]redmineValue{
		"Devices": "iPhone, iPad",
		"Note":    "handheld",
		"Empty":   "",
	}
	if len(iss.CustomFields) != len(wantFields) {
		t.Fatalf("expected %d custom fields, got %d", len(wantFields), len(iss.CustomFields))
	}
	for _, f := range iss.CustomFields {
		want, ok := wantFields[f.Name]
		if !ok {
			t.Errorf("unexpected custom field %q", f.Name)
			continue
		}
		if f.Value != want {
			t.Errorf("custom field %q: got %q, want %q", f.Name, f.Value, want)
		}
	}

	detail := iss.Journals[0].Details[0]
	if detail.OldValue != "iPhone" || detail.NewValue != "iPhone, iPad" {
		t.Errorf("journal detail values: got old=%q new=%q", detail.OldValue, detail.NewValue)
	}

	// The rendered Markdown must carry the joined multi-value field, and the
	// null-valued field must be skipped rather than rendered as "Empty=".
	_, md := renderIssue(&iss, "https://redmine.example.com", "deviceon-feedback", true, nil)
	if !strings.Contains(md, "Custom fields: Devices=iPhone, iPad, Note=handheld") {
		t.Errorf("rendered markdown missing joined custom fields:\n%s", md)
	}
	if strings.Contains(md, "Empty=") {
		t.Errorf("null-valued custom field should be skipped:\n%s", md)
	}
}
