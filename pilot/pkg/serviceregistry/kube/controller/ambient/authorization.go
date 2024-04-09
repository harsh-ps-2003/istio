// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ambient

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"istio.io/api/security/v1beta1"
	securityclient "istio.io/client-go/pkg/apis/security/v1beta1"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/config/schema/kind"
	"istio.io/istio/pkg/log"
	"istio.io/istio/pkg/util/sets"
	"istio.io/istio/pkg/workloadapi/security"
)

const (
	staticStrictPolicyName = "istio_converted_static_strict" // use '_' character since those are illegal in k8s names
)

func (a *index) Policies(requested sets.Set[model.ConfigKey]) []model.WorkloadAuthorization {
	// TODO: use many Gets instead of List?
	cfgs := a.authorizationPolicies.List()
	l := len(cfgs)
	if len(requested) > 0 {
		l = len(requested)
	}
	res := make([]model.WorkloadAuthorization, 0, l)
	for _, cfg := range cfgs {
		k := model.ConfigKey{
			Kind:      kind.AuthorizationPolicy,
			Name:      cfg.Authorization.Name,
			Namespace: cfg.Authorization.Namespace,
		}

		if len(requested) > 0 && !requested.Contains(k) {
			continue
		}
		res = append(res, cfg)
	}
	return res
}

// convertedSelectorPeerAuthentications returns a list of keys corresponding to one or both of:
// [static STRICT policy, port-level STRICT policy] based on the effective PeerAuthentication policy
func convertedSelectorPeerAuthentications(rootNamespace string, configs []*securityclient.PeerAuthentication) []string {
	var meshCfg, namespaceCfg, workloadCfg *securityclient.PeerAuthentication
	for _, cfg := range configs {
		spec := &cfg.Spec
		if spec.Selector == nil || len(spec.Selector.MatchLabels) == 0 {
			// Namespace-level or mesh-level policy
			if cfg.Namespace == rootNamespace {
				if meshCfg == nil || cfg.CreationTimestamp.Before(&meshCfg.CreationTimestamp) {
					log.Debugf("Switch selected mesh policy to %s.%s (%v)", cfg.Name, cfg.Namespace, cfg.CreationTimestamp)
					meshCfg = cfg
				}
			} else {
				if namespaceCfg == nil || cfg.CreationTimestamp.Before(&namespaceCfg.CreationTimestamp) {
					log.Debugf("Switch selected namespace policy to %s.%s (%v)", cfg.Name, cfg.Namespace, cfg.CreationTimestamp)
					namespaceCfg = cfg
				}
			}
		} else if cfg.Namespace != rootNamespace {
			if workloadCfg == nil || cfg.CreationTimestamp.Before(&workloadCfg.CreationTimestamp) {
				log.Debugf("Switch selected workload policy to %s.%s (%v)", cfg.Name, cfg.Namespace, cfg.CreationTimestamp)
				workloadCfg = cfg
			}
		}
	}

	// Whether it comes from a mesh-wide, namespace-wide, or workload-specific policy
	// if the effective policy is STRICT, then reference our static STRICT policy
	var isEffectiveStrictPolicy bool
	// Only 1 per port workload policy can be effective at a time. In the case of a conflict
	// the oldest policy wins.
	var effectivePortLevelPolicyKey string

	// Process in mesh, namespace, workload order to resolve inheritance (UNSET)
	if meshCfg != nil {
		if !isMtlsModeUnset(meshCfg.Spec.Mtls) {
			isEffectiveStrictPolicy = isMtlsModeStrict(meshCfg.Spec.Mtls)
		}
	}

	if namespaceCfg != nil {
		if !isMtlsModeUnset(namespaceCfg.Spec.Mtls) {
			isEffectiveStrictPolicy = isMtlsModeStrict(namespaceCfg.Spec.Mtls)
		}
	}

	if workloadCfg == nil {
		return effectivePeerAuthenticationKeys(rootNamespace, isEffectiveStrictPolicy, "")
	}

	workloadSpec := &workloadCfg.Spec

	// Regardless of if we have port-level overrides, if the workload policy is STRICT, then we need to reference our static STRICT policy
	if isMtlsModeStrict(workloadSpec.Mtls) {
		isEffectiveStrictPolicy = true
	}

	// Regardless of if we have port-level overrides, if the workload policy is PERMISSIVE or DISABLE, then we shouldn't send our static STRICT policy
	if isMtlsModePermissive(workloadSpec.Mtls) || isMtlsModeDisable(workloadSpec.Mtls) {
		isEffectiveStrictPolicy = false
	}

	if workloadSpec.PortLevelMtls != nil {
		switch workloadSpec.GetMtls().GetMode() {
		case v1beta1.PeerAuthentication_MutualTLS_STRICT:
			foundPermissive := false
			for _, portMtls := range workloadSpec.PortLevelMtls {
				if isMtlsModePermissive(portMtls) || isMtlsModeDisable(portMtls) {
					foundPermissive = true
					break
				}
			}

			if foundPermissive {
				// If we found a non-strict policy, we need to reference this workload policy to see the port level exceptions
				effectivePortLevelPolicyKey = workloadCfg.Namespace + "/" + model.GetAmbientPolicyConfigName(model.ConfigKey{
					Name:      workloadCfg.Name,
					Kind:      kind.PeerAuthentication,
					Namespace: workloadCfg.Namespace,
				})
				isEffectiveStrictPolicy = false // don't send our static STRICT policy since the converted form of this policy will include the default STRICT mode
			}
		case v1beta1.PeerAuthentication_MutualTLS_PERMISSIVE, v1beta1.PeerAuthentication_MutualTLS_DISABLE:
			foundStrict := false
			for _, portMtls := range workloadSpec.PortLevelMtls {
				if isMtlsModeStrict(portMtls) {
					foundStrict = true
					break
				}
			}

			// There's a STRICT port mode, so we need to reference this policy in the workload
			if foundStrict {
				effectivePortLevelPolicyKey = workloadCfg.Namespace + "/" + model.GetAmbientPolicyConfigName(model.ConfigKey{
					Name:      workloadCfg.Name,
					Kind:      kind.PeerAuthentication,
					Namespace: workloadCfg.Namespace,
				})
			}
		default: // Unset
			if isEffectiveStrictPolicy {
				// Strict mesh or namespace policy
				foundPermissive := false
				for _, portMtls := range workloadSpec.PortLevelMtls {
					if isMtlsModePermissive(portMtls) {
						foundPermissive = true
						break
					}
				}

				if foundPermissive {
					// If we found a non-strict policy, we need to reference this workload policy to see the port level exceptions
					effectivePortLevelPolicyKey = workloadCfg.Namespace + "/" + model.GetAmbientPolicyConfigName(model.ConfigKey{
						Name:      workloadCfg.Name,
						Kind:      kind.PeerAuthentication,
						Namespace: workloadCfg.Namespace,
					})
				}
			} else {
				// Permissive mesh or namespace policy
				isEffectiveStrictPolicy = false // any ports that aren't specified will be PERMISSIVE so this workload isn't effectively under a STRICT policy
				foundStrict := false
				for _, portMtls := range workloadSpec.PortLevelMtls {
					if isMtlsModeStrict(portMtls) {
						foundStrict = true
						continue
					}
				}

				// There's a STRICT port mode, so we need to reference this policy in the workload
				if foundStrict {
					effectivePortLevelPolicyKey = workloadCfg.Namespace + "/" + model.GetAmbientPolicyConfigName(model.ConfigKey{
						Name:      workloadCfg.Name,
						Kind:      kind.PeerAuthentication,
						Namespace: workloadCfg.Namespace,
					})
				}
			}
		}
	}

	return effectivePeerAuthenticationKeys(rootNamespace, isEffectiveStrictPolicy, effectivePortLevelPolicyKey)
}

func effectivePeerAuthenticationKeys(rootNamespace string, isEffectiveStringPolicy bool, effectiveWorkloadPolicyKey string) []string {
	res := sets.New[string]()

	if isEffectiveStringPolicy {
		res.Insert(fmt.Sprintf("%s/%s", rootNamespace, staticStrictPolicyName))
	}

	if effectiveWorkloadPolicyKey != "" {
		res.Insert(effectiveWorkloadPolicyKey)
	}

	return sets.SortedList(res)
}

// convertPeerAuthentication converts a PeerAuthentication to an L4 authorization policy (i.e. security.Authorization) iff
// 1. the PeerAuthentication has a workload selector
// 2. The PeerAuthentication is NOT in the root namespace
// 3. There is a portLevelMtls policy (technically implied by 1)
// 4. If the top-level mode is PERMISSIVE or DISABLE, there is at least one portLevelMtls policy with mode STRICT
//
// STRICT policies that don't have portLevelMtls will be
// handled when the Workload xDS resource is pushed (a static STRICT-equivalent policy will always be pushed)
func convertPeerAuthentication(rootNamespace string, cfg *securityclient.PeerAuthentication) *security.Authorization {
	pa := &cfg.Spec

	mode := pa.GetMtls().GetMode()

	scope := security.Scope_WORKLOAD_SELECTOR
	// violates case #1, #2, or #3
	if cfg.Namespace == rootNamespace || pa.Selector == nil || len(pa.PortLevelMtls) == 0 {
		log.Debugf("skipping PeerAuthentication %s/%s for ambient since it isn't a workload policy with port level mTLS", cfg.Namespace, cfg.Name)
		return nil
	}

	action := security.Action_DENY
	var rules []*security.Rules

	if mode == v1beta1.PeerAuthentication_MutualTLS_STRICT {
		rules = append(rules, &security.Rules{
			Matches: []*security.Match{
				{
					NotPrincipals: []*security.StringMatch{
						{
							MatchType: &security.StringMatch_Presence{},
						},
					},
				},
			},
		})
	}

	// If we have a strict policy and all of the ports are strict, it's effectively a strict policy
	// so we can exit early and have the WorkloadRbac xDS server push its static strict policy.
	// Note that this doesn't actually attach the policy to any workload; it just makes it available
	// to ztunnel in case a workload needs it.
	foundNonStrictPortmTLS := false
	for port, mtls := range pa.PortLevelMtls {
		switch portMtlsMode := mtls.GetMode(); {
		case portMtlsMode == v1beta1.PeerAuthentication_MutualTLS_STRICT:
			rules = append(rules, &security.Rules{
				Matches: []*security.Match{
					{
						NotPrincipals: []*security.StringMatch{
							{
								MatchType: &security.StringMatch_Presence{},
							},
						},
						DestinationPorts: []uint32{port},
					},
				},
			})
		case portMtlsMode == v1beta1.PeerAuthentication_MutualTLS_PERMISSIVE:
			// Check top-level mode
			if mode == v1beta1.PeerAuthentication_MutualTLS_PERMISSIVE || mode == v1beta1.PeerAuthentication_MutualTLS_DISABLE {
				// we don't care; log and continue
				log.Debugf("skipping port %s/%s for PeerAuthentication %s/%s for ambient since the parent mTLS mode is %s",
					port, portMtlsMode, cfg.Namespace, cfg.Name, mode)
				continue
			}
			foundNonStrictPortmTLS = true

			// If the top level policy is STRICT, we need to add a rule for the port that exempts it from the deny policy
			rules = append(rules, &security.Rules{
				Matches: []*security.Match{
					{
						NotDestinationPorts: []uint32{port}, // if the incoming connection does not match this port, deny (notice there's no principals requirement)
					},
				},
			})
		case portMtlsMode == v1beta1.PeerAuthentication_MutualTLS_DISABLE:
			// Check top-level mode
			if mode == v1beta1.PeerAuthentication_MutualTLS_PERMISSIVE || mode == v1beta1.PeerAuthentication_MutualTLS_DISABLE {
				// we don't care; log and continue
				log.Debugf("skipping port %s/%s for PeerAuthentication %s/%s for ambient since the parent mTLS mode is %s",
					port, portMtlsMode, cfg.Namespace, cfg.Name, mode)
				continue
			}
			foundNonStrictPortmTLS = true

			// If the top level policy is STRICT, we need to add a rule for the port that exempts it from the deny policy
			rules = append(rules, &security.Rules{
				Matches: []*security.Match{
					{
						NotDestinationPorts: []uint32{port}, // if the incoming connection does not match this port, deny (notice there's no principals requirement)
					},
				},
			})
		default:
			log.Debugf("skipping port %s for PeerAuthentication %s/%s for ambient since it is %s", port, cfg.Namespace, cfg.Name, portMtlsMode)
			continue
		}
	}

	// If the top level TLS mode is STRICT and all of the port level mTLS modes are STRICT, this is just a strict policy and we'll exit early
	if mode == v1beta1.PeerAuthentication_MutualTLS_STRICT && !foundNonStrictPortmTLS {
		return nil
	}

	if len(rules) == 0 {
		// we never added any rules; return
		return nil
	}

	opol := &security.Authorization{
		Name: model.GetAmbientPolicyConfigName(model.ConfigKey{
			Name:      cfg.Name,
			Kind:      kind.PeerAuthentication,
			Namespace: cfg.Namespace,
		}),
		Namespace: cfg.Namespace,
		Scope:     scope,
		Action:    action,
		Groups:    []*security.Group{{Rules: rules}},
	}

	return opol
}

func convertAuthorizationPolicy(rootns string, obj *securityclient.AuthorizationPolicy) *security.Authorization {
	pol := &obj.Spec

	polTargetRef := pol.GetTargetRef()
	if polTargetRef != nil &&
		polTargetRef.Group == gvk.KubernetesGateway.Group &&
		polTargetRef.Kind == gvk.KubernetesGateway.Kind {
		// we have a policy targeting a gateway, do not configure a WDS authorization
		return nil
	}

	scope := security.Scope_WORKLOAD_SELECTOR
	if pol.GetSelector() == nil {
		scope = security.Scope_NAMESPACE
		// TODO: TDA
		if rootns == obj.Namespace {
			scope = security.Scope_GLOBAL // TODO: global workload?
		}
	}
	action := security.Action_ALLOW
	switch pol.Action {
	case v1beta1.AuthorizationPolicy_ALLOW:
	case v1beta1.AuthorizationPolicy_DENY:
		action = security.Action_DENY
	default:
		return nil
	}
	opol := &security.Authorization{
		Name:      obj.Name,
		Namespace: obj.Namespace,
		Scope:     scope,
		Action:    action,
		Groups:    nil,
	}

	for _, rule := range pol.Rules {
		rules := handleRule(action, rule)
		if rules != nil {
			rg := &security.Group{
				Rules: rules,
			}
			opol.Groups = append(opol.Groups, rg)
		}
	}

	return opol
}

func anyNonEmpty[T any](arr ...[]T) bool {
	for _, a := range arr {
		if len(a) > 0 {
			return true
		}
	}
	return false
}

func handleRule(action security.Action, rule *v1beta1.Rule) []*security.Rules {
	toMatches := []*security.Match{}
	for _, to := range rule.To {
		op := to.Operation
		if action == security.Action_ALLOW && anyNonEmpty(op.Hosts, op.NotHosts, op.Methods, op.NotMethods, op.Paths, op.NotPaths) {
			// L7 policies never match for ALLOW
			// For DENY they will always match, so it is more restrictive
			return nil
		}
		match := &security.Match{
			DestinationPorts:    stringToPort(op.Ports),
			NotDestinationPorts: stringToPort(op.NotPorts),
		}
		toMatches = append(toMatches, match)
	}
	fromMatches := []*security.Match{}
	for _, from := range rule.From {
		op := from.Source
		if action == security.Action_ALLOW && anyNonEmpty(op.RemoteIpBlocks, op.NotRemoteIpBlocks, op.RequestPrincipals, op.NotRequestPrincipals) {
			// L7 policies never match for ALLOW
			// For DENY they will always match, so it is more restrictive
			return nil
		}
		match := &security.Match{
			SourceIps:     stringToIP(op.IpBlocks),
			NotSourceIps:  stringToIP(op.NotIpBlocks),
			Namespaces:    stringToMatch(op.Namespaces),
			NotNamespaces: stringToMatch(op.NotNamespaces),
			Principals:    stringToMatch(op.Principals),
			NotPrincipals: stringToMatch(op.NotPrincipals),
		}
		fromMatches = append(fromMatches, match)
	}

	rules := []*security.Rules{}
	if len(toMatches) > 0 {
		rules = append(rules, &security.Rules{Matches: toMatches})
	}
	if len(fromMatches) > 0 {
		rules = append(rules, &security.Rules{Matches: fromMatches})
	}
	for _, when := range rule.When {
		l4 := l4WhenAttributes.Contains(when.Key)
		if action == security.Action_ALLOW && !l4 {
			// L7 policies never match for ALLOW
			// For DENY they will always match, so it is more restrictive
			return nil
		}
		positiveMatch := &security.Match{
			Namespaces:       whenMatch("source.namespace", when, false, stringToMatch),
			Principals:       whenMatch("source.principal", when, false, stringToMatch),
			SourceIps:        whenMatch("source.ip", when, false, stringToIP),
			DestinationPorts: whenMatch("destination.port", when, false, stringToPort),
			DestinationIps:   whenMatch("destination.ip", when, false, stringToIP),

			NotNamespaces:       whenMatch("source.namespace", when, true, stringToMatch),
			NotPrincipals:       whenMatch("source.principal", when, true, stringToMatch),
			NotSourceIps:        whenMatch("source.ip", when, true, stringToIP),
			NotDestinationPorts: whenMatch("destination.port", when, true, stringToPort),
			NotDestinationIps:   whenMatch("destination.ip", when, true, stringToIP),
		}
		rules = append(rules, &security.Rules{Matches: []*security.Match{positiveMatch}})
	}
	return rules
}

var l4WhenAttributes = sets.New(
	"source.ip",
	"source.namespace",
	"source.principal",
	"destination.ip",
	"destination.port",
)

func whenMatch[T any](s string, when *v1beta1.Condition, invert bool, f func(v []string) []T) []T {
	if when.Key != s {
		return nil
	}
	if invert {
		return f(when.NotValues)
	}
	return f(when.Values)
}

func stringToMatch(rules []string) []*security.StringMatch {
	res := make([]*security.StringMatch, 0, len(rules))
	for _, v := range rules {
		var sm *security.StringMatch
		switch {
		case v == "*":
			sm = &security.StringMatch{MatchType: &security.StringMatch_Presence{}}
		case strings.HasPrefix(v, "*"):
			sm = &security.StringMatch{MatchType: &security.StringMatch_Suffix{
				Suffix: strings.TrimPrefix(v, "*"),
			}}
		case strings.HasSuffix(v, "*"):
			sm = &security.StringMatch{MatchType: &security.StringMatch_Prefix{
				Prefix: strings.TrimSuffix(v, "*"),
			}}
		default:
			sm = &security.StringMatch{MatchType: &security.StringMatch_Exact{
				Exact: v,
			}}
		}
		res = append(res, sm)
	}
	return res
}

func stringToPort(rules []string) []uint32 {
	res := make([]uint32, 0, len(rules))
	for _, m := range rules {
		p, err := strconv.ParseUint(m, 10, 32)
		if err != nil || p > 65535 {
			continue
		}
		res = append(res, uint32(p))
	}
	return res
}

func stringToIP(rules []string) []*security.Address {
	res := make([]*security.Address, 0, len(rules))
	for _, m := range rules {
		if len(m) == 0 {
			continue
		}

		var (
			ipAddr        netip.Addr
			maxCidrPrefix uint32
		)

		if strings.Contains(m, "/") {
			ipp, err := netip.ParsePrefix(m)
			if err != nil {
				continue
			}
			ipAddr = ipp.Addr()
			maxCidrPrefix = uint32(ipp.Bits())
		} else {
			ipa, err := netip.ParseAddr(m)
			if err != nil {
				continue
			}

			ipAddr = ipa
			maxCidrPrefix = uint32(ipAddr.BitLen())
		}

		res = append(res, &security.Address{
			Address: ipAddr.AsSlice(),
			Length:  maxCidrPrefix,
		})
	}
	return res
}

func isMtlsModeUnset(mtls *v1beta1.PeerAuthentication_MutualTLS) bool {
	return mtls == nil || mtls.Mode == v1beta1.PeerAuthentication_MutualTLS_UNSET
}

func isMtlsModeStrict(mtls *v1beta1.PeerAuthentication_MutualTLS) bool {
	return mtls != nil && mtls.Mode == v1beta1.PeerAuthentication_MutualTLS_STRICT
}

func isMtlsModeDisable(mtls *v1beta1.PeerAuthentication_MutualTLS) bool {
	return mtls != nil && mtls.Mode == v1beta1.PeerAuthentication_MutualTLS_DISABLE
}

func isMtlsModePermissive(mtls *v1beta1.PeerAuthentication_MutualTLS) bool {
	return mtls != nil && mtls.Mode == v1beta1.PeerAuthentication_MutualTLS_PERMISSIVE
}
