package main

import "testing"

func TestGovernedPolicyExplicitlyEmptyDeniesEveryOperation(t *testing.T) {
	policy, err := parseGovernedPolicy([]byte(`{"namespaces":{}}`))
	if err != nil {
		t.Fatalf("load retired governed tenant policy: %v", err)
	}
	for operation := range governedOperations {
		for _, namespace := range []string{"nimmly", "ambrovia", "hyperidle", ""} {
			if err := policy.Authorize(namespace, "default", operation); err == nil {
				t.Fatalf("empty policy allowed %s for %q", operation, namespace)
			}
		}
	}
}

func TestGovernedPolicyRejectsMissingNamespaces(t *testing.T) {
	for _, input := range []string{`{}`, `{"namespaces":null}`, `null`} {
		if _, err := parseGovernedPolicy([]byte(input)); err == nil {
			t.Fatalf("accepted missing namespaces: %s", input)
		}
	}
}

func TestGovernedPolicyUsesExactServiceAccountRoles(t *testing.T) {
	policy := GovernedPolicy{Namespaces: map[string]GovernedNamespacePolicy{
		"nimmly": {
			ServiceAccounts: map[string][]GovernedOperation{
				"transformation-storage-authority":  {OperationRegisterObject, OperationDeleteObject, OperationIssueGrant, OperationConsumeGrant, OperationRevokeGrant},
				"transformation-sandbox-controller": {OperationConsumeGrant, OperationRevokeGrant, OperationRegisterOutput},
			},
		},
	}}

	tests := []struct {
		name      string
		namespace string
		account   string
		operation GovernedOperation
		allowed   bool
	}{
		{"issuer registers objects", "nimmly", "transformation-storage-authority", OperationRegisterObject, true},
		{"issuer deletes exact objects", "nimmly", "transformation-storage-authority", OperationDeleteObject, true},
		{"issuer issues grants", "nimmly", "transformation-storage-authority", OperationIssueGrant, true},
		{"issuer consumes bounded grants", "nimmly", "transformation-storage-authority", OperationConsumeGrant, true},
		{"controller consumes grants", "nimmly", "transformation-sandbox-controller", OperationConsumeGrant, true},
		{"controller registers outputs", "nimmly", "transformation-sandbox-controller", OperationRegisterOutput, true},
		{"controller cannot issue", "nimmly", "transformation-sandbox-controller", OperationIssueGrant, false},
		{"controller cannot delete", "nimmly", "transformation-sandbox-controller", OperationDeleteObject, false},
		{"other service account denied", "nimmly", "default", OperationConsumeGrant, false},
		{"other tenant denied", "ambrovia", "transformation-sandbox-controller", OperationConsumeGrant, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := policy.Authorize(test.namespace, test.account, test.operation)
			if test.allowed && err != nil {
				t.Fatalf("Authorize() error = %v", err)
			}
			if !test.allowed && err == nil {
				t.Fatal("Authorize() unexpectedly allowed operation")
			}
		})
	}
}

func TestGovernedPolicyRejectsUnknownOperationAtLoad(t *testing.T) {
	_, err := parseGovernedPolicy([]byte(`{
		"namespaces": {
			"nimmly": {
				"serviceAccounts": {
					"worker": ["grant:issue", "bucket:admin"]
				}
			}
		}
	}`))
	if err == nil {
		t.Fatal("parseGovernedPolicy() accepted an unknown operation")
	}
}

func TestGovernedPolicyRejectsTrailingJSONAtLoad(t *testing.T) {
	_, err := parseGovernedPolicy([]byte(`{
		"namespaces": {
			"nimmly": {
				"serviceAccounts": {
					"worker": ["grant:consume"]
				}
			}
		}
	} {}`))
	if err == nil {
		t.Fatal("parseGovernedPolicy() accepted trailing JSON")
	}
}
