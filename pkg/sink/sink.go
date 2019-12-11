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

package sink

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1alpha1"
	"io/ioutil"
	"k8s.io/apimachinery/pkg/watch"
	"net/http"
	"path"
	"sort"

	"strings"

	"github.com/tektoncd/triggers/pkg/interceptors/github"
	"github.com/tektoncd/triggers/pkg/interceptors/gitlab"

	"github.com/tektoncd/triggers/pkg/interceptors"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/xerrors"

	"github.com/tektoncd/triggers/pkg/interceptors/webhook"

	pipelineclientset "github.com/tektoncd/pipeline/pkg/client/clientset/versioned"
	triggersclientset "github.com/tektoncd/triggers/pkg/client/clientset/versioned"

	triggersv1 "github.com/tektoncd/triggers/pkg/apis/triggers/v1alpha1"
	"github.com/tektoncd/triggers/pkg/template"
	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	discoveryclient "k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	githubsuites "github.com/google/go-github/github"
)

const (
	PipelineLabelInjectedKey  = "app"
	GithubAccessTokenSecretKey = "accessToken"
	RepoStatusPending = "pending"
	RepoStatusSuccess = "success"
	RepoStatusError = "error"
	RepoStatusFailure = "failure"
	)
// Sink defines the sink resource for processing incoming events for the
// EventListener.
type Sink struct {
	KubeClientSet          kubernetes.Interface
	TriggersClient         triggersclientset.Interface
	DiscoveryClient        discoveryclient.DiscoveryInterface
	RESTClient             restclient.Interface
	PipelineClient         pipelineclientset.Interface
	HTTPClient             *http.Client
	EventListenerName      string
	EventListenerNamespace string
	Logger                 *zap.SugaredLogger
}


// HandleEvent processes an incoming HTTP event for the event listener.
func (r Sink) HandleEvent(response http.ResponseWriter, request *http.Request) {
	el, err := r.TriggersClient.TektonV1alpha1().EventListeners(r.EventListenerNamespace).Get(r.EventListenerName, metav1.GetOptions{})

	if err != nil {
		r.Logger.Fatalf("Error getting EventListener %s in Namespace %s: %s", r.EventListenerName, r.EventListenerNamespace, err)
		response.WriteHeader(http.StatusInternalServerError)
		return
	}

	event, err := ioutil.ReadAll(request.Body)
	if err != nil {
		r.Logger.Errorf("Error reading event body: %s", err)
		response.WriteHeader(http.StatusInternalServerError)
		return
	}

	eventID := template.UID()
	eventLog := r.Logger.With(zap.String(triggersv1.EventIDLabelKey, eventID))
	eventLog.Debugf("EventListener: %s in Namespace: %s handling event (EventID: %s) with payload: %s and header: %v",
		r.EventListenerName, r.EventListenerNamespace, eventID, string(event), request.Header)

	result := make(chan int, 10)
	// Execute each Trigger
	var repoName, orgName, commitSha string
	for _, t := range el.Spec.Triggers {
		log := eventLog.With(zap.String(triggersv1.TriggerLabelKey, t.Name))

		var interceptor interceptors.Interceptor
		if t.Interceptor != nil {
			switch {
			case t.Interceptor.Webhook != nil:
				interceptor = webhook.NewInterceptor(t.Interceptor.Webhook, r.HTTPClient, r.EventListenerNamespace, log)
			case t.Interceptor.Github != nil:
				interceptor = github.NewInterceptor(t.Interceptor.Github, r.KubeClientSet, r.EventListenerNamespace, log)
			case t.Interceptor.Gitlab != nil:
				interceptor = gitlab.NewInterceptor(t.Interceptor.Gitlab, r.KubeClientSet, r.EventListenerNamespace, log)
			}
		}
		go func(t triggersv1.EventListenerTrigger) {
			finalPayload := event
			if interceptor != nil {
				payload, err := interceptor.ExecuteTrigger(event, request, &t, eventID)

				if err != nil {
					log.Error(err)
					result <- http.StatusAccepted
					return
				}
				finalPayload = payload
				repoName, orgName, commitSha = getGithubInfoFromPayload(finalPayload)
			}
			rt, err := template.ResolveTrigger(t,
				r.TriggersClient.TektonV1alpha1().TriggerBindings(r.EventListenerNamespace).Get,
				r.TriggersClient.TektonV1alpha1().TriggerTemplates(r.EventListenerNamespace).Get)
			if err != nil {
				log.Error(err)
				result <- http.StatusAccepted
				return
			}

			pipeline, err := r.PipelineClient.TektonV1alpha1().Pipelines(
				r.EventListenerNamespace).Get(getPipelineNameFromTriggerTemplate(rt.TriggerTemplate),
					metav1.GetOptions{})

			github.PostGithubStatusChecks(
				context.Background(),
				getAccessToken(r.KubeClientSet,el, r.EventListenerNamespace, log),
				RepoStatusPending,
				orgName, repoName, commitSha,
				getPipelineTasks(pipeline), log)

			params, err := template.ResolveParams(rt.TriggerBindings, finalPayload, request.Header, rt.TriggerTemplate.Spec.Params)
			if err != nil {
				//log.Errorf("err ======> %+v\n", err)
				result <- http.StatusAccepted
				return
			}
			log.Info("params: %+v", params)
			resources := template.ResolveResources(rt.TriggerTemplate, params)
			res, err := r.createResources(resources, t.Name, eventID)
			if  err != nil {
				log.Error(err)
			}
			result <- http.StatusCreated
			a, err := res.Raw()
			fmt.Printf("res.Raw() ==============> %v\n", string(a))
		}(t)
	}

	//The eventlistener waits until all the trigger executions (up-to the creation of the resources) and
	//only when at least one of the execution completed successfully, it returns response code 201(Accepted) otherwise it returns 202 (Created).
	code := http.StatusAccepted
	for i := 0; i < len(el.Spec.Triggers); i++ {
		thiscode := <-result
		if thiscode < code {
			code = thiscode
		}
	}
	/////////
	watch := make(chan watch.Interface)
	w, err := r.PipelineClient.TektonV1alpha1().PipelineRuns(r.EventListenerNamespace).Watch(metav1.ListOptions{})
	watch <- w
	/////////
	// TODO: Do we really need to return the entire body back???
	response.WriteHeader(code)
	//fmt.Fprintf(response, "EventListener: %s in Namespace: %s handling event (EventID: %s) with payload: %s and header: %v",
	//	r.EventListenerName, r.EventListenerNamespace, string(eventID), string(event), request.Header)
}

func (r Sink) createResources(resources []json.RawMessage, triggerName, eventID string) (result *restclient.Result, err error) {
	for _, resource := range resources {
		if result, err = r.createResource(resource, triggerName, eventID); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// createResource uses the kubeClient to create the resource defined in the
// TriggerResourceTemplate and returns any errors with this process
func (r Sink) createResource(rt json.RawMessage, triggerName string, eventID string) (*restclient.Result, error) {
	// Assume the TriggerResourceTemplate is valid (it has an apiVersion and Kind)
	apiVersion := gjson.GetBytes(rt, "apiVersion").String()
	kind := gjson.GetBytes(rt, "kind").String()
	namespace := gjson.GetBytes(rt, "metadata.namespace").String()


	// Default the resource creation to the EventListenerNamespace if not found in the resource template
	if namespace == "" {
		namespace = r.EventListenerNamespace
	}
	apiResource, err := findAPIResource(r.DiscoveryClient, apiVersion, kind)
	if err != nil {
		return nil, err
	}

	rt, err = addLabels(rt, map[string]string{
		triggersv1.EventListenerLabelKey: r.EventListenerName,
		triggersv1.EventIDLabelKey:       eventID,
		triggersv1.TriggerLabelKey:       triggerName,
		PipelineLabelInjectedKey  : 	  "nabil",
	})
	if err != nil {
		return nil, err
	}

	resourcename := gjson.GetBytes(rt, "metadata.name")
	resourcekind := gjson.GetBytes(rt, "kind")
	r.Logger.Infof("Generating resource: kind: %s, name: %s ", resourcekind, resourcename)

	uri := createRequestURI(apiVersion, apiResource.Name, namespace, apiResource.Namespaced)

	result := r.RESTClient.Post().
		RequestURI(uri).
		Body([]byte(rt)).
		SetHeader("Content-Type", "application/json").
		Do()
	if result.Error() != nil {
		return nil, result.Error()
	}
	return &result, nil
}

// findAPIResource returns the APIResource definition using the discovery client.
func findAPIResource(discoveryClient discoveryclient.DiscoveryInterface, apiVersion, kind string) (*metav1.APIResource, error) {
	resourceList, err := discoveryClient.ServerResourcesForGroupVersion(apiVersion)

	if err != nil {
		return nil, xerrors.Errorf("Error getting kubernetes server resources for apiVersion %s: %s", apiVersion, err)
	}
	for _, apiResource := range resourceList.APIResources {
		if apiResource.Kind == kind {
			return &apiResource, nil
		}
	}
	return nil, xerrors.Errorf("Error could not find resource with apiVersion %s and kind %s", apiVersion, kind)
}

// createRequestURI returns the URI for a request to the kubernetes API REST endpoint.
// If namespaced is false, then namespace will be excluded from the URI.
func createRequestURI(apiVersion, namePlural, namespace string, namespaced bool) string {
	var uri string
	if apiVersion == "v1" {
		uri = "api/v1"
	} else {
		uri = path.Join(uri, "apis", apiVersion)
	}
	if namespaced {
		uri = path.Join(uri, "namespaces", namespace)
	}
	uri = path.Join(uri, namePlural)
	return uri
}

// addLabels adds autogenerated Tekton labels to created resources.
func addLabels(rt json.RawMessage, labels map[string]string) (json.RawMessage, error) {
	var err error
	for k, v := range labels {
		l := fmt.Sprintf("metadata.labels.%s/%s", triggersv1.LabelEscape, strings.TrimLeft(k, "/"))
		rt, err = sjson.SetBytes(rt, l, v)
		if err != nil {
			return rt, err
		}
	}
	return rt, err
}

func getPipelineNameFromTriggerTemplate(tt *triggersv1.TriggerTemplate) string{
	var retStr string
	for _, resourceTemplate := range tt.Spec.ResourceTemplates {
		pr := &v1alpha1.PipelineRun{}
		err := json.Unmarshal(resourceTemplate.RawMessage, pr)
		if err != nil{
			return ""
		}
		if pr.Kind == "PipelineRun"{
			retStr = pr.Spec.PipelineRef.Name
		}
	}
	return retStr
}

func getPipelineTasks(pipeline *v1alpha1.Pipeline)[]string{
	var retStrs []string
	for _, task := range pipeline.Spec.Tasks {
		retStrs = append(retStrs, task.Name)
	}
	sort.Reverse(sort.StringSlice(retStrs))
	return retStrs
}

func getAccessToken(ki kubernetes.Interface, listener *triggersv1.EventListener, ns string, log *zap.SugaredLogger)string{
	serviceAccount, err := ki.CoreV1().ServiceAccounts(ns).Get(listener.Spec.ServiceAccountName, metav1.GetOptions{})
	if err != nil{
		log.Error(err)
		return ""
	}
	for _, secret := range serviceAccount.Secrets {
		secret, err := ki.CoreV1().Secrets(ns).Get(secret.Name, metav1.GetOptions{})
		if err != nil{
			log.Error(err)
			return ""
		}
		for key, bytes := range secret.Data {
			if strings.Contains(key, GithubAccessTokenSecretKey){
				return string(bytes)
			}
		}
	}
	return ""
}

func getGithubInfoFromPayload(payload []byte) (repoName , orgName, commitId string){
	wp := &githubsuites.WebHookPayload{}
	err := json.Unmarshal(payload, wp)
	if err == nil {
		return *wp.Repo.Name, strings.Split(*wp.Repo.FullName, "/")[0], *wp.After
	}
	return "", "", ""
}