// Copyright (c) 2026 WSO2 LLC. (https://www.wso2.com).
//
// WSO2 LLC. licenses this file to you under the Apache License,
// Version 2.0 (the "License"); you may not use this file except
// in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/wso2-open-operations/cs-tools/entity-service/internal/apierror"
	"github.com/wso2-open-operations/cs-tools/entity-service/internal/auth"
	"github.com/wso2-open-operations/cs-tools/entity-service/internal/domain"
	"github.com/wso2-open-operations/cs-tools/entity-service/internal/events"
)

// TestIncidentPriorityFor pins ServiceNow's stock impact x urgency matrix,
// including LOW/LOW -> PLANNING (migration 0099).
func TestIncidentPriorityFor(t *testing.T) {
	tests := []struct {
		impact  domain.IncidentImpact
		urgency domain.IncidentUrgency
		want    string
	}{
		{domain.IncidentImpactHigh, domain.IncidentUrgencyHigh, "CRITICAL"},
		{domain.IncidentImpactHigh, domain.IncidentUrgencyMedium, "HIGH"},
		{domain.IncidentImpactHigh, domain.IncidentUrgencyLow, "MODERATE"},
		{domain.IncidentImpactMedium, domain.IncidentUrgencyHigh, "HIGH"},
		{domain.IncidentImpactMedium, domain.IncidentUrgencyMedium, "MODERATE"},
		{domain.IncidentImpactMedium, domain.IncidentUrgencyLow, "LOW"},
		{domain.IncidentImpactLow, domain.IncidentUrgencyHigh, "MODERATE"},
		{domain.IncidentImpactLow, domain.IncidentUrgencyMedium, "LOW"},
		{domain.IncidentImpactLow, domain.IncidentUrgencyLow, "PLANNING"},
	}
	for _, tt := range tests {
		if got := incidentPriorityFor(tt.impact, tt.urgency); got != tt.want {
			t.Errorf("incidentPriorityFor(%s, %s) = %s, want %s", tt.impact, tt.urgency, got, tt.want)
		}
	}
}

// TestIncidentService_CreateIncidentPortal_PassesDerivedFieldsAndPublishes
// covers the plain-Postgres create: priority and the subcategory's
// ServiceNow value are derived before the insert, a configuration item is
// accepted (no longer a 400), and incident.created fires once the insert
// has succeeded.
func TestIncidentService_CreateIncidentPortal_PassesDerivedFieldsAndPublishes(t *testing.T) {
	var gotPriority, gotCreatedBy string
	var gotSubcategory *string
	var gotReq domain.CreateIncidentRequest
	repo := &stubIncidentRepo{
		supportGroups: requestServiceExists(),
		createIncident: func(_ context.Context, req domain.CreateIncidentRequest, priority string, subcategoryValue *string, createdBy string) (domain.CreateIncidentResponse, error) {
			gotReq, gotPriority, gotSubcategory, gotCreatedBy = req, priority, subcategoryValue, createdBy
			resp := domain.CreateIncidentResponse{Message: "Incident created successfully."}
			resp.Incident.ID = "66666666-6666-6666-6666-666666666666"
			return resp, nil
		},
		// The publish this path makes is the enriched one, which reads the incident back to fill
		// the escalation fields the ladder routes on (and panics on the bare stub without this).
		getIncidentByID: func(_ context.Context, id string) (domain.IncidentView, error) {
			v := newTestIncidentView(id)
			v.AssignmentGroup = &domain.EntityRef{ID: "88888888-8888-8888-8888-888888888888", Name: "SRE - Apollo"}
			return v, nil
		},
	}
	publisher := &mockEventPublisher{}
	svc := NewIncidentService(repo, publisher)

	req := validCreateIncidentRequest()
	req.Category = domain.IncidentCategoryServiceInterruption
	sub := domain.IncidentSubcategoryPartialOutage
	req.Subcategory = &sub
	req.Impact = domain.IncidentImpactHigh
	req.Urgency = domain.IncidentUrgencyMedium
	ci := "77777777-7777-7777-7777-777777777777"
	req.ConfigurationItemID = &ci

	ctx := auth.WithIdentity(context.Background(), auth.Identity{Validated: true, UserEmail: "jane.doe@example.com"})
	if _, err := svc.CreateIncident(ctx, req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotPriority != "HIGH" {
		t.Errorf("priority = %q, want HIGH", gotPriority)
	}
	if gotSubcategory == nil || *gotSubcategory != "Partial Outage" {
		t.Errorf("subcategory value = %v, want \"Partial Outage\"", gotSubcategory)
	}
	if gotCreatedBy != "jane.doe@example.com" {
		t.Errorf("createdBy = %q, want the caller's email", gotCreatedBy)
	}
	if gotReq.ConfigurationItemID == nil || *gotReq.ConfigurationItemID != ci {
		t.Errorf("configurationItemId not passed through to the repository")
	}
	if len(publisher.calls) != 1 || publisher.calls[0].eventType != events.TypeIncidentCreated || publisher.calls[0].entityID != "66666666-6666-6666-6666-666666666666" {
		t.Fatalf("expected one incident.created publish for the new id, got %+v", publisher.calls)
	}
	// The call-escalation ladders route on the assignment group, so the Postgres create's event
	// must carry it, read back from the incident it just stored.
	var payload events.IncidentCreatedPayload
	if err := json.Unmarshal(publisher.calls[0].payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Team != "SRE - Apollo" {
		t.Errorf("incident.created team = %q, want the stored assignment group", payload.Team)
	}
}

// TestIncidentService_CreateIncidentPortal_WithoutSubcategory: subcategory is
// optional on create, so a request with none passes validation and reaches
// the repository with a nil subcategory value (-> incident.subcategory_id
// NULL), rather than an empty-string lookup that would fail to resolve.
func TestIncidentService_CreateIncidentPortal_WithoutSubcategory(t *testing.T) {
	called := false
	var gotSubcategory *string
	repo := &stubIncidentRepo{
		supportGroups: requestServiceExists(),
		createIncident: func(_ context.Context, req domain.CreateIncidentRequest, _ string, subcategoryValue *string, _ string) (domain.CreateIncidentResponse, error) {
			called = true
			gotSubcategory = subcategoryValue
			if req.Subcategory != nil {
				t.Errorf("req.Subcategory = %v, want nil", *req.Subcategory)
			}
			resp := domain.CreateIncidentResponse{Message: "Incident created successfully."}
			resp.Incident.ID = "66666666-6666-6666-6666-666666666666"
			return resp, nil
		},
	}
	svc := NewIncidentService(repo, nil)

	req := validCreateIncidentRequest()
	req.Category = domain.IncidentCategoryServiceInterruption
	req.Subcategory = nil

	ctx := auth.WithIdentity(context.Background(), auth.Identity{Validated: true, UserEmail: "jane.doe@example.com"})
	if _, err := svc.CreateIncident(ctx, req); err != nil {
		t.Fatalf("CreateIncident without subcategory: unexpected error: %v", err)
	}
	if !called {
		t.Fatal("repository CreateIncident was not reached")
	}
	if gotSubcategory != nil {
		t.Errorf("subcategory value = %q, want nil", *gotSubcategory)
	}
}

// TestIncidentService_CreateIncidentPortal_MachineCallerUsesClientID guards
// the alert-ingestion case: a machine caller has no user token, and must not
// be refused for it.
func TestIncidentService_CreateIncidentPortal_MachineCallerUsesClientID(t *testing.T) {
	var gotCreatedBy string
	repo := &stubIncidentRepo{
		supportGroups: requestServiceExists(),
		createIncident: func(_ context.Context, _ domain.CreateIncidentRequest, _ string, _ *string, createdBy string) (domain.CreateIncidentResponse, error) {
			gotCreatedBy = createdBy
			return domain.CreateIncidentResponse{}, nil
		},
	}
	svc := NewIncidentService(repo, nil)

	ctx := auth.WithIdentity(context.Background(), auth.Identity{Validated: true, ClientID: "alert-ingestion"})
	if _, err := svc.CreateIncident(ctx, validCreateIncidentRequest()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotCreatedBy != "alert-ingestion" {
		t.Errorf("createdBy = %q, want the client id", gotCreatedBy)
	}
}

// TestIncidentService_CreateIncidentPortal_ValidatesBeforeInsert: the same
// input the ServiceNow path rejects is rejected here, before Postgres.
func TestIncidentService_CreateIncidentPortal_ValidatesBeforeInsert(t *testing.T) {
	svc := NewIncidentService(&stubIncidentRepo{supportGroups: requestServiceExists()}, nil) // CreateIncident panics if reached

	cases := map[string]func(*domain.CreateIncidentRequest){
		"missing subject":  func(r *domain.CreateIncidentRequest) { r.Subject = "" },
		"bad impact":       func(r *domain.CreateIncidentRequest) { r.Impact = "SEVERE" },
		"bad caller uuid":  func(r *domain.CreateIncidentRequest) { r.CallerID = "not-a-uuid" },
		"environment > 40": func(r *domain.CreateIncidentRequest) { e := string(make([]byte, 41)); r.Environment = &e },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := validCreateIncidentRequest()
			mutate(&req)
			_, err := svc.CreateIncident(context.Background(), req)
			var ve *apierror.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("expected *apierror.ValidationError, got %T: %v", err, err)
			}
		})
	}
}

// TestIncidentService_CreateIncidentPortal_NoPublishOnFailure: a failed
// insert must not announce an incident that does not exist.
func TestIncidentService_CreateIncidentPortal_NoPublishOnFailure(t *testing.T) {
	repo := &stubIncidentRepo{
		supportGroups: requestServiceExists(),
		createIncident: func(context.Context, domain.CreateIncidentRequest, string, *string, string) (domain.CreateIncidentResponse, error) {
			return domain.CreateIncidentResponse{}, errors.New("insert failed")
		},
	}
	publisher := &mockEventPublisher{}
	svc := NewIncidentService(repo, publisher)

	if _, err := svc.CreateIncident(context.Background(), validCreateIncidentRequest()); err == nil {
		t.Fatal("expected the repository error")
	}
	if len(publisher.calls) != 0 {
		t.Errorf("expected no publish after a failed insert, got %d", len(publisher.calls))
	}
}
