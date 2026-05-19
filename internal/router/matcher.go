package router

import (
	"errors"
	"sort"
	"strings"

	"github.com/calmlax/aevons-gateway/internal/model"
)

type ServiceMatcher struct {
	rules []model.ServiceRule
}

func NewServiceMatcher(services []model.ServiceConfig) (*ServiceMatcher, error) {
	rules := make([]model.ServiceRule, 0, len(services))
	for _, service := range services {
		matchPrefix := normalizePrefix(service.Prefix)
		if service.ID == "" || service.Name == "" || matchPrefix == "" {
			return nil, errors.New("gateway service config requires id, name and prefix")
		}

		excludes, err := compileRouteRules(service.ExcludeAuthRoutes)
		if err != nil {
			return nil, err
		}

		rules = append(rules, model.ServiceRule{
			ID:               strings.TrimSpace(service.ID),
			Name:             strings.TrimSpace(service.Name),
			Prefix:           strings.TrimSpace(service.Prefix),
			MatchPrefix:      matchPrefix,
			Discovery:        strings.TrimSpace(strings.ToLower(service.Discovery)),
			LoadBalance:      strings.TrimSpace(strings.ToLower(service.LoadBalance)),
			PassAccessToken:  service.PassAccessToken,
			ExcludeAuthRules: excludes,
		})
	}

	sort.Slice(rules, func(i, j int) bool {
		return len(rules[i].MatchPrefix) > len(rules[j].MatchPrefix)
	})

	return &ServiceMatcher{rules: rules}, nil
}

func (m *ServiceMatcher) Match(path string) (*model.ServiceRule, bool) {
	for i := range m.rules {
		if strings.HasPrefix(path, m.rules[i].MatchPrefix) {
			return &m.rules[i], true
		}
	}
	return nil, false
}

func (m *ServiceMatcher) Rules() []*model.ServiceRule {
	if m == nil {
		return nil
	}
	rules := make([]*model.ServiceRule, 0, len(m.rules))
	for i := range m.rules {
		rules = append(rules, &m.rules[i])
	}
	return rules
}

func normalizePrefix(prefix string) string {
	trimmed := strings.TrimSpace(prefix)
	trimmed = strings.TrimSuffix(trimmed, "**")
	trimmed = strings.TrimSuffix(trimmed, "*")
	trimmed = strings.TrimSuffix(trimmed, "/")
	if trimmed == "" {
		return ""
	}
	return trimmed + "/"
}

func compileRouteRules(raw []string) ([]model.RouteRule, error) {
	rules := make([]model.RouteRule, 0, len(raw))
	for _, item := range raw {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}

		parts := strings.SplitN(item, ":", 2)
		if len(parts) != 2 {
			return nil, errors.New("invalid route rule: " + item)
		}

		method := strings.ToUpper(strings.TrimSpace(parts[0]))
		pattern := strings.TrimSpace(parts[1])
		if method == "" || pattern == "" {
			return nil, errors.New("invalid route rule: " + item)
		}
		if method != "ANY" &&
			method != "GET" &&
			method != "POST" &&
			method != "PUT" &&
			method != "DELETE" &&
			method != "PATCH" &&
			method != "HEAD" &&
			method != "OPTIONS" {
			return nil, errors.New("unsupported route rule method: " + item)
		}

		rules = append(rules, model.RouteRule{
			Method:    method,
			Pattern:   strings.TrimSuffix(pattern, "**"),
			IsPrefix:  strings.HasSuffix(pattern, "**"),
			RawSource: item,
		})
	}
	return rules, nil
}

func IsExcluded(rule *model.ServiceRule, method, path string) bool {
	if rule == nil {
		return false
	}
	method = strings.ToUpper(method)
	for _, item := range rule.ExcludeAuthRules {
		if item.Method != "ANY" && item.Method != method {
			continue
		}
		if item.IsPrefix && strings.HasPrefix(path, item.Pattern) {
			return true
		}
		if !item.IsPrefix && path == item.Pattern {
			return true
		}
	}
	return false
}
