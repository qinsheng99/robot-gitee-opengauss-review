package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	sdk "github.com/opensourceways/go-gitee/gitee"
	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	msgPRConflicts        = "PR conflicts to the target branch."
	msgMissingLabels      = "PR does not have these lables: %s"
	msgInvalidLabels      = "PR should remove these labels: %s"
	msgNotEnoughLGTMLabel = "PR needs %d lgtm labels and now gets %d"
)

var regCheckPr = regexp.MustCompile(`(?mi)^/check-pr\s*$`)

func (bot *robot) handleCheckPR(e *sdk.NoteEvent, cfg *botConfig) error {
	if !e.IsPullRequest() ||
		!e.IsPROpen() ||
		!e.IsCreatingCommentEvent() ||
		!regCheckPr.MatchString(e.GetComment().GetBody()) {
		return nil
	}

	pr := e.GetPullRequest()
	org, repo := e.GetOrgRepo()

	ops, err := bot.cli.ListPROperationLogs(org, repo, pr.GetNumber())
	if err != nil {
		return err
	}

	if r := canMerge(pr.Mergeable, e.GetPRLabelSet(), cfg, ops); len(r) > 0 {
		return bot.cli.CreatePRComment(
			org, repo, e.GetPRNumber(),
			fmt.Sprintf(
				"@%s , this pr is not mergeable and the reasons are below:\n%s",
				e.GetCommenter(), strings.Join(r, "\n"),
			),
		)
	}

	return bot.mergePR(
		pr.NeedReview || pr.NeedTest,
		org, repo, e.GetPRNumber(), string(cfg.MergeMethod),
	)
}

func (bot *robot) tryMerge(e *sdk.PullRequestEvent, cfg *botConfig) error {
	if sdk.GetPullRequestAction(e) != sdk.PRActionUpdatedLabel {
		return nil
	}

	pr := e.PullRequest
	org, repo := e.GetOrgRepo()

	ops, err := bot.cli.ListPROperationLogs(org, repo, pr.GetNumber())
	if err != nil {
		return err
	}

	if r := canMerge(pr.GetMergeable(), e.GetPRLabelSet(), cfg, ops); len(r) > 0 {
		return nil
	}

	return bot.mergePR(
		pr.GetNeedReview() || pr.GetNeedTest(),
		org, repo, e.GetPRNumber(), string(cfg.MergeMethod),
	)
}

func (bot *robot) mergePR(needReviewOrTest bool, org, repo string, number int32, method string) error {
	if needReviewOrTest {
		v := int32(0)
		p := sdk.PullRequestUpdateParam{
			AssigneesNumber: &v,
			TestersNumber:   &v,
		}
		if _, err := bot.cli.UpdatePullRequest(org, repo, number, p); err != nil {
			return err
		}
	}

	return bot.cli.MergePR(
		org, repo, number,
		sdk.PullRequestMergePutParam{
			MergeMethod: method,
		},
	)
}

func canMerge(mergeable bool, labels sets.String, cfg *botConfig, ops []sdk.OperateLog) []string {
	if !mergeable {
		return []string{msgPRConflicts}
	}

	reasons := []string{}

	needs := sets.NewString(approvedLabel)
	needs.Insert(cfg.LabelsForMerge...)

	if ln := cfg.LgtmCountsRequired; ln == 1 {
		needs.Insert(lgtmLabel)
	} else {
		v := getLGTMLabelsOnPR(labels)
		if n := uint(len(v)); n < ln {
			reasons = append(reasons, fmt.Sprintf(msgNotEnoughLGTMLabel, ln, n))
		}
	}

	if v := needs.Difference(labels); v.Len() > 0 {
		reasons = append(reasons, fmt.Sprintf(
			msgMissingLabels, strings.Join(v.UnsortedList(), ", "),
		))
	}

	if len(cfg.MissingLabelsForMerge) > 0 {
		missing := sets.NewString(cfg.MissingLabelsForMerge...)
		if v := missing.Intersection(labels); v.Len() > 0 {
			reasons = append(reasons, fmt.Sprintf(
				msgInvalidLabels, strings.Join(v.UnsortedList(), ", "),
			))
		}
	}

	// check who add these labels
	if r := isLabelMatched(labels, cfg, ops); len(r) > 0 {
		reasons = append(reasons, r...)
	}

	return reasons
}

func isLabelMatched(labels sets.String, cfg *botConfig, ops []sdk.OperateLog) []string {
	var reasons []string

	needs := sets.NewString(approvedLabel)
	needs.Insert(cfg.LabelsForMerge...)

	if ln := cfg.LgtmCountsRequired; ln == 1 {
		needs.Insert(lgtmLabel)
	} else {
		v := getLGTMLabelsOnPR(labels)
		if n := uint(len(v)); n < ln {
			reasons = append(reasons, fmt.Sprintf(msgNotEnoughLGTMLabel, ln, n))
		}
	}

	s := checkLabelsLegal(labels, needs, ops, cfg.LegalOperator)
	if s != "" {
		reasons = append(reasons, s)
	}

	if v := needs.Difference(labels); v.Len() > 0 {
		reasons = append(reasons, fmt.Sprintf(
			msgMissingLabels, strings.Join(v.UnsortedList(), ", "),
		))
	}

	if len(cfg.MissingLabelsForMerge) > 0 {
		missing := sets.NewString(cfg.MissingLabelsForMerge...)
		if v := missing.Intersection(labels); v.Len() > 0 {
			reasons = append(reasons, fmt.Sprintf(
				msgInvalidLabels, strings.Join(v.UnsortedList(), ", "),
			))
		}
	}

	return reasons
}

type labelLog struct {
	label string
	who   string
	t     time.Time
}

func getLatestLog(ops []sdk.OperateLog, label string) (labelLog, bool) {
	var t time.Time

	index := -1

	for i := range ops {
		op := &ops[i]

		if op.ActionType != sdk.ActionAddLabel || !strings.Contains(op.Content, label) {
			continue
		}

		ut, err := time.Parse(time.RFC3339, op.CreatedAt)
		if err != nil {
			// log.Warnf("parse time:%s failed", op.CreatedAt)

			continue
		}

		if index < 0 || ut.After(t) {
			t = ut
			index = i
		}
	}

	if index >= 0 {
		if user := ops[index].User; user != nil && user.Login != "" {
			return labelLog{
				label: label,
				t:     t,
				who:   user.Login,
			}, true
		}
	}

	return labelLog{}, false
}

func checkLabelsLegal(labels sets.String, needs sets.String, ops []sdk.OperateLog, legalOperator string) string {
	f := func(label string) string {
		v, b := getLatestLog(ops, label)
		if !b {
			return fmt.Sprintf("The corresponding operation log is missing. you should delete " +
				"the label and add it again by correct way")
		}

		if v.who != legalOperator {
			if strings.HasPrefix(v.label, "opengauss-cla/") {
				return fmt.Sprintf("%s You can't add %s by yourself, "+
					"please remove it and use /check-cla to add it", v.who, v.label)
			}

			return fmt.Sprintf("%s You can't add %s by yourself, please contact the maintainers", v.who, v.label)
		}

		return ""
	}

	v := make([]string, 0, len(labels))

	for label := range labels {
		if ok := needs.Has(label); ok || strings.HasPrefix(label, lgtmLabel) {
			if s := f(label); s != "" {
				v = append(v, fmt.Sprintf("%s: %s", label, s))
			}
		}
	}

	if n := len(v); n > 0 {
		s := "label is"

		if n > 1 {
			s = "labels are"
		}

		return fmt.Sprintf("**The following %s not ready**.\n\n%s", s, strings.Join(v, "\n\n"))
	}

	return ""
}
