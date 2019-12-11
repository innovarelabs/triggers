/*
Copyright 2019 The Tekton Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

		http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package github

import (
	"errors"
	"fmt"
	"golang.org/x/net/context"
	"golang.org/x/oauth2"
	"net/http"

	gh "github.com/google/go-github/github"

	"github.com/tektoncd/triggers/pkg/interceptors"

	triggersv1 "github.com/tektoncd/triggers/pkg/apis/triggers/v1alpha1"

	"go.uber.org/zap"
	"k8s.io/client-go/kubernetes"
)

const (
	GithubBranchToProtect = "master"
)

type Interceptor struct {
	KubeClientSet          kubernetes.Interface
	Logger                 *zap.SugaredLogger
	Github                 *triggersv1.GithubInterceptor
	EventListenerNamespace string
}

func NewInterceptor(gh *triggersv1.GithubInterceptor, k kubernetes.Interface, ns string, l *zap.SugaredLogger) interceptors.Interceptor {
	return &Interceptor{
		Logger:                 l,
		Github:                 gh,
		KubeClientSet:          k,
		EventListenerNamespace: ns,
	}
}

func (w *Interceptor) ExecuteTrigger(payload []byte, request *http.Request, _ *triggersv1.EventListenerTrigger, _ string) ([]byte, error) {
	// Validate secrets first before anything else, if set
	if w.Github.SecretRef != nil {
		header := request.Header.Get("X-Hub-Signature")
		if header == "" {
			return nil, errors.New("no X-Hub-Signature header set")
		}

		secretToken, err := interceptors.GetSecretToken(w.KubeClientSet, w.Github.SecretRef, w.EventListenerNamespace)

		if err != nil {
			return nil, err
		}
		if err := gh.ValidateSignature(header, payload, secretToken); err != nil {
			return nil, err
		}
	}

	// Next see if the event type is in the allow-list
	if w.Github.EventTypes != nil {
		actualEvent := request.Header.Get("X-Github-Event")
		isAllowed := false
		for _, allowedEvent := range w.Github.EventTypes {
			if actualEvent == allowedEvent {
				isAllowed = true
				break
			}
		}
		if !isAllowed {
			return nil, fmt.Errorf("event type %s is not allowed", actualEvent)
		}
	}

	return payload, nil
}

func PostGithubStatusChecks(ctx context.Context, ghAccessToken, status, orgName, repoName, commitSha string, tasksToRestrict []string, log *zap.SugaredLogger) {
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: ghAccessToken},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := gh.NewClient(tc)
	for _, taskName := range tasksToRestrict {
		rs := &gh.RepoStatus{
			Context: &taskName,
			State:   &[]string{status}[0],
		}
		//TODO - Need to add check if the value are empty
		_, _, err := client.Repositories.CreateStatus(ctx, orgName, repoName, commitSha, rs)
		if err != nil {
			log.Errorf("err ======> %+v\n", err)
		}
	}
	protectionRequest := &gh.ProtectionRequest{
		RequiredStatusChecks: &gh.RequiredStatusChecks{
			Strict:   true,
			Contexts: tasksToRestrict,
		},
		RequiredPullRequestReviews: nil,
		EnforceAdmins:              false,
		Restrictions:               nil,
	}
	_, _, err := client.Repositories.UpdateBranchProtection(context.Background(), orgName, repoName, GithubBranchToProtect, protectionRequest)
	if err != nil {
		log.Errorf("err ======> %+v\n", err)
	}
}
