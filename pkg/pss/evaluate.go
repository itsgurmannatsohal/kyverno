package pss

import (
	"fmt"
	"regexp"
	"strconv"

	kyvernov1 "github.com/kyverno/kyverno/api/kyverno/v1"
	"github.com/kyverno/kyverno/ext/wildcard"
	pssutils "github.com/kyverno/kyverno/pkg/pss/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/pod-security-admission/api"
	"k8s.io/pod-security-admission/policy"
)

var (
	regexIndex = regexp.MustCompile(`\d+`)
	regexStr   = regexp.MustCompile(`[a-zA-Z]+`)
)

// Evaluate Pod's specified containers only and get PSSCheckResults
func evaluatePSS(level *api.LevelVersion, pod corev1.Pod) (results []pssutils.PSSCheckResult) {
	checks := policy.DefaultChecks()
	var latestVersionCheck policy.VersionedCheck
	for _, check := range checks {
		if level.Level == api.LevelBaseline && check.Level != level.Level {
			continue
		}

		latestVersionCheck = check.Versions[0]
		for i := 1; i < len(check.Versions); i++ {
			vc := check.Versions[i]
			if !vc.MinimumVersion.Older(latestVersionCheck.MinimumVersion) {
				latestVersionCheck = vc
			}
		}

		if level.Version == api.LatestVersion() {
			checkResult := latestVersionCheck.CheckPod(&pod.ObjectMeta, &pod.Spec, policy.WithFieldErrors())
			if !checkResult.Allowed {
				results = append(results, pssutils.PSSCheckResult{
					ID:               string(check.ID),
					CheckResult:      checkResult,
					RestrictedFields: GetRestrictedFields(check),
				})
			}
		}

		for _, versionCheck := range check.Versions {
			// the latest check returned twice, skip duplicate application
			if level.Version == api.LatestVersion() {
				continue
			} else if level.Version != api.LatestVersion() && level.Version.Older(versionCheck.MinimumVersion) {
				continue
			}
			checkResult := versionCheck.CheckPod(&pod.ObjectMeta, &pod.Spec, policy.WithFieldErrors())
			// Append only if the checkResult is not already in pssCheckResult
			if !checkResult.Allowed {
				results = append(results, pssutils.PSSCheckResult{
					ID:               string(check.ID),
					CheckResult:      checkResult,
					RestrictedFields: GetRestrictedFields(check),
				})
			}
		}
	}
	return results
}

func exemptExclusions(defaultCheckResults, excludeCheckResults []pssutils.PSSCheckResult, exclude kyvernov1.PodSecurityStandard, pod *corev1.Pod, matching *corev1.Pod, isContainerLevelExclusion bool) ([]pssutils.PSSCheckResult, error) {
	defaultCheckResultsMap := make(map[string]pssutils.PSSCheckResult, len(defaultCheckResults))

	if err := exclude.Validate(exclude); err != nil {
		fmt.Print(err)
		return nil, err
	}

	for _, result := range defaultCheckResults {
		defaultCheckResultsMap[result.ID] = result
	}

	for _, excludeResult := range excludeCheckResults {
		for _, checkID := range pssutils.PSS_controls_to_check_id[exclude.ControlName] {
			if excludeResult.ID == checkID {
				for _, excludeFieldErr := range *excludeResult.CheckResult.ErrList {
					var excludeField, excludeContainerType string
					var excludeIndexes []int
					var isContainerLevelField bool = false
					var excludeContainer corev1.Container

					if isContainerLevelExclusion {
						excludeField, excludeIndexes, excludeContainerType, isContainerLevelField = parseField(excludeFieldErr.Field)
					} else {
						excludeField = regexIndex.ReplaceAllString(excludeFieldErr.Field, "*")
					}

					if isContainerLevelField {
						excludeContainer = getContainerInfo(matching, excludeIndexes[0], excludeContainerType)
					}
					excludeBadValues := extractBadValues(excludeFieldErr)

					if excludeField == exclude.RestrictedField || len(exclude.RestrictedField) == 0 {
						flag := true
						if len(exclude.Values) != 0 {
							for _, badValue := range excludeBadValues {
								if !wildcard.CheckPatterns(exclude.Values, badValue) {
									flag = false
									break
								}
							}
						}
						if flag {
							defaultCheckResult := defaultCheckResultsMap[checkID]
							if defaultCheckResult.CheckResult.ErrList != nil {
								for idx, defaultFieldErr := range *defaultCheckResult.CheckResult.ErrList {
									var defaultField, defaultContainerType string
									var defaultIndexes []int
									var isContainerLevelField bool = false
									var defaultContainer corev1.Container

									if isContainerLevelExclusion {
										defaultField, defaultIndexes, defaultContainerType, isContainerLevelField = parseField(defaultFieldErr.Field)
									} else {
										defaultField = regexIndex.ReplaceAllString(defaultFieldErr.Field, "*")
									}

									if isContainerLevelField {
										defaultContainer = getContainerInfo(pod, defaultIndexes[0], defaultContainerType)
										if excludeField == defaultField && excludeContainer.Name == defaultContainer.Name {
											remove(defaultCheckResult.CheckResult.ErrList, idx)
											break
										}
									} else {
										if excludeField == defaultField {
											remove(defaultCheckResult.CheckResult.ErrList, idx)
											break
										}
									}
								}
								if len(*defaultCheckResult.CheckResult.ErrList) == 0 {
									delete(defaultCheckResultsMap, checkID)
								} else {
									defaultCheckResultsMap[checkID] = defaultCheckResult
								}
							}
						}
					}
				}
			}
		}
	}

	var newDefaultCheckResults []pssutils.PSSCheckResult
	for _, result := range defaultCheckResultsMap {
		newDefaultCheckResults = append(newDefaultCheckResults, result)
	}

	return newDefaultCheckResults, nil
}

func extractBadValues(excludeFieldErr *field.Error) []string {
	var excludeBadValues []string

	switch excludeFieldErr.BadValue.(type) {
	case string:
		badValue := excludeFieldErr.BadValue.(string)
		if badValue == "" {
			break
		}
		excludeBadValues = append(excludeBadValues, badValue)
	case bool:
		excludeBadValues = append(excludeBadValues, strconv.FormatBool(excludeFieldErr.BadValue.(bool)))
	case int:
		excludeBadValues = append(excludeBadValues, strconv.Itoa(excludeFieldErr.BadValue.(int)))
	case []string:
		excludeBadValues = append(excludeBadValues, excludeFieldErr.BadValue.([]string)...)
	}

	return excludeBadValues
}

func remove(s *field.ErrorList, i int) {
	(*s)[i] = (*s)[len(*s)-1]
	*s = (*s)[:len(*s)-1]
}

func isContainerType(str string) bool {
	return str == "containers" || str == "initContainers" || str == "ephemeralContainers"
}

func parseField(field string) (string, []int, string, bool) {
	matchesIdx := regexIndex.FindAllStringSubmatch(field, -1)
	matchesStr := regexStr.FindAllString(field, -1)
	field = regexIndex.ReplaceAllString(field, "*")
	var indexes []int
	for _, match := range matchesIdx {
		index, _ := strconv.Atoi(match[0])
		indexes = append(indexes, index)
	}
	return field, indexes, matchesStr[1], isContainerType(matchesStr[1])
}

func getContainerInfo(pod *corev1.Pod, index int, containerType string) corev1.Container {
	var container corev1.Container

	switch {
	case containerType == "containers":
		container = pod.Spec.Containers[index]
	case containerType == "initContainers":
		container = pod.Spec.InitContainers[index]
	case containerType == "ephemeralContainers":
		container = (corev1.Container)(pod.Spec.EphemeralContainers[index].EphemeralContainerCommon)
	default:
	}

	return container
}

func parseVersion(rule *kyvernov1.PodSecurity) (*api.LevelVersion, error) {
	// Get pod security admission version
	var apiVersion api.Version

	// Version set to "latest" by default
	if rule.Version == "" || rule.Version == "latest" {
		apiVersion = api.LatestVersion()
	} else {
		parsedApiVersion, err := api.ParseVersion(rule.Version)
		if err != nil {
			return nil, err
		}
		apiVersion = api.MajorMinorVersion(parsedApiVersion.Major(), parsedApiVersion.Minor())
	}
	return &api.LevelVersion{
		Level:   rule.Level,
		Version: apiVersion,
	}, nil
}

// EvaluatePod applies PSS checks to the pod and exempts controls specified in the rule
func EvaluatePod(rule *kyvernov1.PodSecurity, pod *corev1.Pod) (bool, []pssutils.PSSCheckResult, error) {
	levelVersion, err := parseVersion(rule)
	if err != nil {
		return false, nil, err
	}

	defaultCheckResults := evaluatePSS(levelVersion, *pod)

	for _, exclude := range rule.Exclude {
		spec, matching := GetPodWithMatchingContainers(exclude, pod)

		switch {
		// exclude pod level checks
		case spec != nil:
			isContainerLevelExclusion := false
			excludeCheckResults := evaluatePSS(levelVersion, *spec)
			defaultCheckResults, err = exemptExclusions(defaultCheckResults, excludeCheckResults, exclude, pod, matching, isContainerLevelExclusion)

		// exclude container level checks
		default:
			isContainerLevelExclusion := true
			excludeCheckResults := evaluatePSS(levelVersion, *matching)
			defaultCheckResults, err = exemptExclusions(defaultCheckResults, excludeCheckResults, exclude, pod, matching, isContainerLevelExclusion)
		}
	}

	return (len(defaultCheckResults) == 0 && err == nil), defaultCheckResults, err
}

// GetPodWithMatchingContainers extracts matching container/pod info by the given exclude rule
// and returns pod manifests containing spec and container info respectively
func GetPodWithMatchingContainers(exclude kyvernov1.PodSecurityStandard, pod *corev1.Pod) (podSpec, matching *corev1.Pod) {
	if len(exclude.Images) == 0 {
		podSpec = pod.DeepCopy()
		podSpec.Spec.Containers = []corev1.Container{{Name: "fake"}}
		podSpec.Spec.InitContainers = nil
		podSpec.Spec.EphemeralContainers = nil
		return podSpec, nil
	}

	matchingImages := exclude.Images
	matching = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.GetName(),
			Namespace: pod.GetNamespace(),
		},
	}
	for _, container := range pod.Spec.Containers {
		if wildcard.CheckPatterns(matchingImages, container.Image) {
			matching.Spec.Containers = append(matching.Spec.Containers, container)
		}
	}
	for _, container := range pod.Spec.InitContainers {
		if wildcard.CheckPatterns(matchingImages, container.Image) {
			matching.Spec.InitContainers = append(matching.Spec.InitContainers, container)
		}
	}

	for _, container := range pod.Spec.EphemeralContainers {
		if wildcard.CheckPatterns(matchingImages, container.Image) {
			matching.Spec.EphemeralContainers = append(matching.Spec.EphemeralContainers, container)
		}
	}

	return nil, matching
}

// Get restrictedFields from Check.ID
func GetRestrictedFields(check policy.Check) []pssutils.RestrictedField {
	for _, control := range pssutils.PSS_controls_to_check_id {
		for _, checkID := range control {
			if string(check.ID) == checkID {
				return pssutils.PSS_controls[checkID]
			}
		}
	}
	return nil
}

func FormatChecksPrint(checks []pssutils.PSSCheckResult) string {
	var str string
	for _, check := range checks {
		str += fmt.Sprintf("\n(Forbidden reason: %s, field error list: [", check.CheckResult.ForbiddenReason)
		for idx, err := range *check.CheckResult.ErrList {
			badValueExist := true
			switch err.BadValue.(type) {
			case string:
				badValue := err.BadValue.(string)
				if badValue == "" {
					badValueExist = false
				}
			default:
			}
			switch err.Type {
			case field.ErrorTypeForbidden:
				if badValueExist {
					str += fmt.Sprintf("%s is forbidden, don't set the BadValue: %+v", err.Field, err.BadValue)
				} else {
					str += err.Error()
				}
			default:
				str += err.Error()
			}
			if idx != len(*check.CheckResult.ErrList)-1 {
				str += ", "
			}
		}
		str += "])"
	}
	return str
}
