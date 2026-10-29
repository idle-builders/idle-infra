package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
)

type GovernedOperation string

const (
	OperationRegisterObject GovernedOperation = "object:register"
	OperationDeleteObject   GovernedOperation = "object:delete"
	OperationIssueGrant     GovernedOperation = "grant:issue"
	OperationRevokeGrant    GovernedOperation = "grant:revoke"
	OperationConsumeGrant   GovernedOperation = "grant:consume"
	OperationRegisterOutput GovernedOperation = "output:register"
	OperationLakeRead       GovernedOperation = "lake:read"
	OperationLakeWrite      GovernedOperation = "lake:write"
	OperationLakeDelete     GovernedOperation = "lake:delete"
)

var governedOperations = map[GovernedOperation]struct{}{
	OperationRegisterObject: {},
	OperationDeleteObject:   {},
	OperationIssueGrant:     {},
	OperationRevokeGrant:    {},
	OperationConsumeGrant:   {},
	OperationRegisterOutput: {},
	OperationLakeRead:       {},
	OperationLakeWrite:      {},
	OperationLakeDelete:     {},
}

type GovernedPolicy struct {
	Namespaces map[string]GovernedNamespacePolicy `json:"namespaces"`
}

type GovernedNamespacePolicy struct {
	ServiceAccounts map[string][]GovernedOperation `json:"serviceAccounts"`
}

var kubernetesName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func parseGovernedPolicy(data []byte) (*GovernedPolicy, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var policy GovernedPolicy
	if err := decoder.Decode(&policy); err != nil {
		return nil, fmt.Errorf("parse governed policy: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse governed policy: expected one JSON value")
	}
	if len(policy.Namespaces) == 0 {
		return nil, fmt.Errorf("governed policy has no namespaces")
	}
	for namespace, namespacePolicy := range policy.Namespaces {
		if !kubernetesName.MatchString(namespace) {
			return nil, fmt.Errorf("governed policy has invalid namespace %q", namespace)
		}
		if len(namespacePolicy.ServiceAccounts) == 0 {
			return nil, fmt.Errorf("governed policy namespace %q has no service accounts", namespace)
		}
		for account, operations := range namespacePolicy.ServiceAccounts {
			if !kubernetesName.MatchString(account) {
				return nil, fmt.Errorf("governed policy has invalid service account %q", account)
			}
			if len(operations) == 0 {
				return nil, fmt.Errorf("governed policy service account %q has no operations", account)
			}
			seen := make(map[GovernedOperation]struct{}, len(operations))
			for _, operation := range operations {
				if _, ok := governedOperations[operation]; !ok {
					return nil, fmt.Errorf("governed policy has unknown operation %q", operation)
				}
				if _, duplicate := seen[operation]; duplicate {
					return nil, fmt.Errorf("governed policy repeats operation %q", operation)
				}
				seen[operation] = struct{}{}
			}
		}
	}
	return &policy, nil
}

func LoadGovernedPolicy(path string) (*GovernedPolicy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read governed policy: %w", err)
	}
	return parseGovernedPolicy(data)
}

func (p *GovernedPolicy) Authorize(namespace, account string, operation GovernedOperation) error {
	namespacePolicy, ok := p.Namespaces[namespace]
	if !ok {
		return deny("tenant not provisioned for governed storage")
	}
	operations, ok := namespacePolicy.ServiceAccounts[account]
	if !ok {
		return deny("service account not provisioned for governed storage")
	}
	for _, allowed := range operations {
		if allowed == operation {
			return nil
		}
	}
	return deny("service account operation not allowed")
}
