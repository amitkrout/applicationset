package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/argoproj-labs/applicationset/pkg/generators"
	"github.com/argoproj-labs/applicationset/pkg/utils"
	"github.com/argoproj/argo-cd/v2/common"
	argov1alpha1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	crtclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	argoprojiov1alpha1 "github.com/argoproj-labs/applicationset/api/v1alpha1"
	appclientset "github.com/argoproj/argo-cd/v2/pkg/client/clientset/versioned/fake"
	dbmocks "github.com/argoproj/argo-cd/v2/util/db/mocks"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

type generatorMock struct {
	mock.Mock
}

func (g *generatorMock) GetTemplate(appSetGenerator *argoprojiov1alpha1.ApplicationSetGenerator) *argoprojiov1alpha1.ApplicationSetTemplate {
	args := g.Called(appSetGenerator)

	return args.Get(0).(*argoprojiov1alpha1.ApplicationSetTemplate)
}

func (g *generatorMock) GenerateParams(appSetGenerator *argoprojiov1alpha1.ApplicationSetGenerator, _ *argoprojiov1alpha1.ApplicationSet) ([]map[string]string, error) {
	args := g.Called(appSetGenerator)

	return args.Get(0).([]map[string]string), args.Error(1)
}

type rendererMock struct {
	mock.Mock
}

func (g *generatorMock) GetRequeueAfter(appSetGenerator *argoprojiov1alpha1.ApplicationSetGenerator) time.Duration {
	args := g.Called(appSetGenerator)

	return args.Get(0).(time.Duration)
}

func (r *rendererMock) RenderTemplateParams(tmpl *argov1alpha1.Application, syncPolicy *argoprojiov1alpha1.ApplicationSetSyncPolicy, params map[string]string) (*argov1alpha1.Application, error) {
	args := r.Called(tmpl, params)

	if args.Error(1) != nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*argov1alpha1.Application), args.Error(1)

}

func TestExtractApplications(t *testing.T) {
	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	for _, c := range []struct {
		name                string
		params              []map[string]string
		template            argoprojiov1alpha1.ApplicationSetTemplate
		generateParamsError error
		rendererError       error
		expectErr           bool
	}{
		{
			name:   "Generate two applications",
			params: []map[string]string{{"name": "app1"}, {"name": "app2"}},
			template: argoprojiov1alpha1.ApplicationSetTemplate{
				ApplicationSetTemplateMeta: argoprojiov1alpha1.ApplicationSetTemplateMeta{
					Name:      "name",
					Namespace: "namespace",
					Labels:    map[string]string{"label_name": "label_value"},
				},
				Spec: argov1alpha1.ApplicationSpec{},
			},
		},
		{
			name:                "Handles error from the generator",
			generateParamsError: errors.New("error"),
			expectErr:           true,
		},
		{
			name:   "Handles error from the render",
			params: []map[string]string{{"name": "app1"}, {"name": "app2"}},
			template: argoprojiov1alpha1.ApplicationSetTemplate{
				ApplicationSetTemplateMeta: argoprojiov1alpha1.ApplicationSetTemplateMeta{
					Name:      "name",
					Namespace: "namespace",
					Labels:    map[string]string{"label_name": "label_value"},
				},
				Spec: argov1alpha1.ApplicationSpec{},
			},
			rendererError: errors.New("error"),
			expectErr:     true,
		},
	} {
		cc := c
		app := argov1alpha1.Application{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test",
			},
		}

		t.Run(cc.name, func(t *testing.T) {

			generatorMock := generatorMock{}
			generator := argoprojiov1alpha1.ApplicationSetGenerator{
				List: &argoprojiov1alpha1.ListGenerator{},
			}

			generatorMock.On("GenerateParams", &generator).
				Return(cc.params, cc.generateParamsError)

			generatorMock.On("GetTemplate", &generator).
				Return(&argoprojiov1alpha1.ApplicationSetTemplate{})

			rendererMock := rendererMock{}

			var expectedApps []argov1alpha1.Application

			if cc.generateParamsError == nil {
				for _, p := range cc.params {

					if cc.rendererError != nil {
						rendererMock.On("RenderTemplateParams", getTempApplication(cc.template), p).
							Return(nil, cc.rendererError)
					} else {
						rendererMock.On("RenderTemplateParams", getTempApplication(cc.template), p).
							Return(&app, nil)
						expectedApps = append(expectedApps, app)
					}
				}
			}

			r := ApplicationSetReconciler{
				Client:   client,
				Scheme:   scheme,
				Recorder: record.NewFakeRecorder(1),
				Generators: map[string]generators.Generator{
					"List": &generatorMock,
				},
				Renderer:      &rendererMock,
				KubeClientset: kubefake.NewSimpleClientset(),
			}

			got, err := r.generateApplications(argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Generators: []argoprojiov1alpha1.ApplicationSetGenerator{generator},
					Template:   cc.template,
				},
			})

			if cc.expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, expectedApps, got)
			generatorMock.AssertNumberOfCalls(t, "GenerateParams", 1)

			if cc.generateParamsError == nil {
				rendererMock.AssertNumberOfCalls(t, "RenderTemplateParams", len(cc.params))
			}

		})
	}

}

func TestMergeTemplateApplications(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = argoprojiov1alpha1.AddToScheme(scheme)
	_ = argov1alpha1.AddToScheme(scheme)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	for _, c := range []struct {
		name             string
		params           []map[string]string
		template         argoprojiov1alpha1.ApplicationSetTemplate
		overrideTemplate argoprojiov1alpha1.ApplicationSetTemplate
		expectedMerged   argoprojiov1alpha1.ApplicationSetTemplate
		expectedApps     []argov1alpha1.Application
	}{
		{
			name:   "Generate app",
			params: []map[string]string{{"name": "app1"}},
			template: argoprojiov1alpha1.ApplicationSetTemplate{
				ApplicationSetTemplateMeta: argoprojiov1alpha1.ApplicationSetTemplateMeta{
					Name:      "name",
					Namespace: "namespace",
					Labels:    map[string]string{"label_name": "label_value"},
				},
				Spec: argov1alpha1.ApplicationSpec{},
			},
			overrideTemplate: argoprojiov1alpha1.ApplicationSetTemplate{
				ApplicationSetTemplateMeta: argoprojiov1alpha1.ApplicationSetTemplateMeta{
					Name:   "test",
					Labels: map[string]string{"foo": "bar"},
				},
				Spec: argov1alpha1.ApplicationSpec{},
			},
			expectedMerged: argoprojiov1alpha1.ApplicationSetTemplate{
				ApplicationSetTemplateMeta: argoprojiov1alpha1.ApplicationSetTemplateMeta{
					Name:      "test",
					Namespace: "namespace",
					Labels:    map[string]string{"label_name": "label_value", "foo": "bar"},
				},
				Spec: argov1alpha1.ApplicationSpec{},
			},
			expectedApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test",
						Namespace: "test",
						Labels:    map[string]string{"foo": "bar"},
					},
					Spec: argov1alpha1.ApplicationSpec{},
				},
			},
		},
	} {
		cc := c

		t.Run(cc.name, func(t *testing.T) {

			generatorMock := generatorMock{}
			generator := argoprojiov1alpha1.ApplicationSetGenerator{
				List: &argoprojiov1alpha1.ListGenerator{},
			}

			generatorMock.On("GenerateParams", &generator).
				Return(cc.params, nil)

			generatorMock.On("GetTemplate", &generator).
				Return(&cc.overrideTemplate)

			rendererMock := rendererMock{}

			rendererMock.On("RenderTemplateParams", getTempApplication(cc.expectedMerged), cc.params[0]).
				Return(&cc.expectedApps[0], nil)

			r := ApplicationSetReconciler{
				Client:   client,
				Scheme:   scheme,
				Recorder: record.NewFakeRecorder(1),
				Generators: map[string]generators.Generator{
					"List": &generatorMock,
				},
				Renderer:      &rendererMock,
				KubeClientset: kubefake.NewSimpleClientset(),
			}

			got, _ := r.generateApplications(argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Generators: []argoprojiov1alpha1.ApplicationSetGenerator{generator},
					Template:   cc.template,
				},
			},
			)

			assert.Equal(t, cc.expectedApps, got)
		})
	}

}

func TestCreateOrUpdateInCluster(t *testing.T) {

	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	for _, c := range []struct {
		// name is human-readable test name
		name string
		// appSet is the ApplicationSet we are generating resources for
		appSet argoprojiov1alpha1.ApplicationSet
		// existingApps are the apps that already exist on the cluster
		existingApps []argov1alpha1.Application
		// desiredApps are the generated apps to create/update
		desiredApps []argov1alpha1.Application
		// expected is what we expect the cluster Applications to look like, after createOrUpdateInCluster
		expected []argov1alpha1.Application
	}{
		{
			name: "Create an app that doesn't exist",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
			},
			existingApps: nil,
			desiredApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "1",
					},
				},
			},
		},
		{
			name: "Update an existing app with a different project name",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			},
			existingApps: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "test",
					},
				},
			},
			desiredApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "3",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
		},
		{
			name: "Create a new app and check it doesn't replace the existing app",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			},
			existingApps: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "test",
					},
				},
			},
			desiredApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app2",
						Namespace:       "namespace",
						ResourceVersion: "1",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
		},
		{
			name: "Ensure that labels and annotations are added (via update) into an exiting application",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			},
			existingApps: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			desiredApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "app1",
						Labels:      map[string]string{"label-key": "label-value"},
						Annotations: map[string]string{"annot-key": "annot-value"},
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						Labels:          map[string]string{"label-key": "label-value"},
						Annotations:     map[string]string{"annot-key": "annot-value"},
						ResourceVersion: "3",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
		},
		{
			name: "Ensure that labels and annotations are removed from an existing app",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			},
			existingApps: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "2",
						Labels:          map[string]string{"label-key": "label-value"},
						Annotations:     map[string]string{"annot-key": "annot-value"},
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			desiredApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "3",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
		},
		{
			name: "Ensure that status and operation fields are not overriden by an update, when removing labels/annotations",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			},
			existingApps: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "2",
						Labels:          map[string]string{"label-key": "label-value"},
						Annotations:     map[string]string{"annot-key": "annot-value"},
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
					Status: argov1alpha1.ApplicationStatus{
						Resources: []argov1alpha1.ResourceStatus{{Name: "sample-name"}},
					},
					Operation: &argov1alpha1.Operation{
						Sync: &argov1alpha1.SyncOperation{Revision: "sample-revision"},
					},
				},
			},
			desiredApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "3",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
					Status: argov1alpha1.ApplicationStatus{
						Resources: []argov1alpha1.ResourceStatus{{Name: "sample-name"}},
					},
					Operation: &argov1alpha1.Operation{
						Sync: &argov1alpha1.SyncOperation{Revision: "sample-revision"},
					},
				},
			},
		},
		{
			name: "Ensure that status and operation fields are not overriden by an update, when removing labels/annotations and adding other fields",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project:     "project",
							Source:      argov1alpha1.ApplicationSource{Path: "path", TargetRevision: "revision", RepoURL: "repoURL"},
							Destination: argov1alpha1.ApplicationDestination{Server: "server", Namespace: "namespace"},
						},
					},
				},
			},
			existingApps: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
					Status: argov1alpha1.ApplicationStatus{
						Resources: []argov1alpha1.ResourceStatus{{Name: "sample-name"}},
					},
					Operation: &argov1alpha1.Operation{
						Sync: &argov1alpha1.SyncOperation{Revision: "sample-revision"},
					},
				},
			},
			desiredApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "app1",
						Labels:      map[string]string{"label-key": "label-value"},
						Annotations: map[string]string{"annot-key": "annot-value"},
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project:     "project",
						Source:      argov1alpha1.ApplicationSource{Path: "path", TargetRevision: "revision", RepoURL: "repoURL"},
						Destination: argov1alpha1.ApplicationDestination{Server: "server", Namespace: "namespace"},
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						Labels:          map[string]string{"label-key": "label-value"},
						Annotations:     map[string]string{"annot-key": "annot-value"},
						ResourceVersion: "3",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project:     "project",
						Source:      argov1alpha1.ApplicationSource{Path: "path", TargetRevision: "revision", RepoURL: "repoURL"},
						Destination: argov1alpha1.ApplicationDestination{Server: "server", Namespace: "namespace"},
					},
					Status: argov1alpha1.ApplicationStatus{
						Resources: []argov1alpha1.ResourceStatus{{Name: "sample-name"}},
					},
					Operation: &argov1alpha1.Operation{
						Sync: &argov1alpha1.SyncOperation{Revision: "sample-revision"},
					},
				},
			},
		},
		{
			name: "Ensure that argocd notifications state annotation is preserved from an existing app",
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			},
			existingApps: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "2",
						Labels:          map[string]string{"label-key": "label-value"},
						Annotations: map[string]string{
							"annot-key":           "annot-value",
							NotifiedAnnotationKey: `{"b620d4600c771a6f4cxxxxxxx:on-deployed:[0].y7b5sbwa2Q329JYHxxxxxx-fBs:slack:slack-test":1617144614}`,
						},
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			desiredApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "3",
						Annotations: map[string]string{
							NotifiedAnnotationKey: `{"b620d4600c771a6f4cxxxxxxx:on-deployed:[0].y7b5sbwa2Q329JYHxxxxxx-fBs:slack:slack-test":1617144614}`,
						},
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
		},
	} {

		t.Run(c.name, func(t *testing.T) {

			initObjs := []crtclient.Object{&c.appSet}

			for _, a := range c.existingApps {
				err = controllerutil.SetControllerReference(&c.appSet, &a, scheme)
				assert.Nil(t, err)
				initObjs = append(initObjs, &a)
			}

			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(initObjs...).Build()

			r := ApplicationSetReconciler{
				Client:   client,
				Scheme:   scheme,
				Recorder: record.NewFakeRecorder(len(initObjs) + len(c.expected)),
			}

			err = r.createOrUpdateInCluster(context.TODO(), c.appSet, c.desiredApps)
			assert.Nil(t, err)

			for _, obj := range c.expected {
				got := &argov1alpha1.Application{}
				_ = client.Get(context.Background(), crtclient.ObjectKey{
					Namespace: obj.Namespace,
					Name:      obj.Name,
				}, got)

				err = controllerutil.SetControllerReference(&c.appSet, &obj, r.Scheme)
				assert.Nil(t, err)
				assert.Equal(t, obj, *got)
			}
		})
	}
}

func TestRemoveFinalizerOnInvalidDestination_FinalizerTypes(t *testing.T) {

	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	for _, c := range []struct {
		// name is human-readable test name
		name               string
		existingFinalizers []string
		expectedFinalizers []string
	}{
		{
			name:               "no finalizers",
			existingFinalizers: []string{},
			expectedFinalizers: nil,
		},
		{
			name:               "contains only argo finalizer",
			existingFinalizers: []string{common.ResourcesFinalizerName},
			expectedFinalizers: nil,
		},
		{
			name:               "contains only non-argo finalizer",
			existingFinalizers: []string{"non-argo-finalizer"},
			expectedFinalizers: []string{"non-argo-finalizer"},
		},
		{
			name:               "contains both argo and non-argo finalizer",
			existingFinalizers: []string{"non-argo-finalizer", common.ResourcesFinalizerName},
			expectedFinalizers: []string{"non-argo-finalizer"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {

			appSet := argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			}

			app := argov1alpha1.Application{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "app1",
					Finalizers: c.existingFinalizers,
				},
				Spec: argov1alpha1.ApplicationSpec{
					Project: "project",
					Source:  argov1alpha1.ApplicationSource{Path: "path", TargetRevision: "revision", RepoURL: "repoURL"},
					// Destination is always invalid, for this test:
					Destination: argov1alpha1.ApplicationDestination{Name: "my-cluster", Namespace: "namespace"},
				},
			}

			initObjs := []crtclient.Object{&app, &appSet}

			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(initObjs...).Build()
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-secret",
					Namespace: "namespace",
					Labels: map[string]string{
						generators.ArgoCDSecretTypeLabel: generators.ArgoCDSecretTypeCluster,
					},
				},
				Data: map[string][]byte{
					// Since this test requires the cluster to be an invalid destination, we
					// always return a cluster named 'my-cluster2' (different from app 'my-cluster', above)
					"name":   []byte("mycluster2"),
					"server": []byte("https://kubernetes.default.svc"),
					"config": []byte("{\"username\":\"foo\",\"password\":\"foo\"}"),
				},
			}

			objects := append([]runtime.Object{}, secret)
			kubeclientset := kubefake.NewSimpleClientset(objects...)

			r := ApplicationSetReconciler{
				Client:        client,
				Scheme:        scheme,
				Recorder:      record.NewFakeRecorder(10),
				KubeClientset: kubeclientset,
			}

			clusterList, err := utils.ListClusters(context.Background(), kubeclientset, "namespace")
			assert.NoError(t, err, "Unexpected error")

			appLog := log.WithFields(log.Fields{"app": app.Name, "appSet": ""})

			appInputParam := app.DeepCopy()

			err = r.removeFinalizerOnInvalidDestination(context.Background(), appSet, appInputParam, clusterList, appLog)
			assert.NoError(t, err, "Unexpected error")

			retrievedApp := argov1alpha1.Application{}
			err = client.Get(context.Background(), crtclient.ObjectKeyFromObject(&app), &retrievedApp)
			assert.NoError(t, err, "Unexpected error")

			// App on the cluster should have the expected finalizers
			assert.ElementsMatch(t, c.expectedFinalizers, retrievedApp.Finalizers)

			// App object passed in as a parameter should have the expected finaliers
			assert.ElementsMatch(t, c.expectedFinalizers, appInputParam.Finalizers)

			bytes, _ := json.MarshalIndent(retrievedApp, "", "  ")
			t.Log("Contents of app after call:", string(bytes))

		})
	}
}

func TestRemoveFinalizerOnInvalidDestination_DestinationTypes(t *testing.T) {

	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	for _, c := range []struct {
		// name is human-readable test name
		name                   string
		destinationField       argov1alpha1.ApplicationDestination
		expectFinalizerRemoved bool
	}{
		{
			name: "invalid cluster: empty destination",
			destinationField: argov1alpha1.ApplicationDestination{
				Namespace: "namespace",
			},
			expectFinalizerRemoved: true,
		},
		{
			name: "invalid cluster: invalid server url",
			destinationField: argov1alpha1.ApplicationDestination{
				Namespace: "namespace",
				Server:    "https://1.2.3.4",
			},
			expectFinalizerRemoved: true,
		},
		{
			name: "invalid cluster: invalid cluster name",
			destinationField: argov1alpha1.ApplicationDestination{
				Namespace: "namespace",
				Name:      "invalid-cluster",
			},
			expectFinalizerRemoved: true,
		},
		{
			name: "invalid cluster by both valid",
			destinationField: argov1alpha1.ApplicationDestination{
				Namespace: "namespace",
				Name:      "mycluster2",
				Server:    "https://kubernetes.default.svc",
			},
			expectFinalizerRemoved: true,
		},
		{
			name: "invalid cluster by both invalid",
			destinationField: argov1alpha1.ApplicationDestination{
				Namespace: "namespace",
				Name:      "mycluster3",
				Server:    "https://4.5.6.7",
			},
			expectFinalizerRemoved: true,
		},
		{
			name: "valid cluster by name",
			destinationField: argov1alpha1.ApplicationDestination{
				Namespace: "namespace",
				Name:      "mycluster2",
			},
			expectFinalizerRemoved: false,
		},
		{
			name: "valid cluster by server",
			destinationField: argov1alpha1.ApplicationDestination{
				Namespace: "namespace",
				Server:    "https://kubernetes.default.svc",
			},
			expectFinalizerRemoved: false,
		},
	} {

		t.Run(c.name, func(t *testing.T) {

			appSet := argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			}

			app := argov1alpha1.Application{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "app1",
					Finalizers: []string{common.ResourcesFinalizerName},
				},
				Spec: argov1alpha1.ApplicationSpec{
					Project:     "project",
					Source:      argov1alpha1.ApplicationSource{Path: "path", TargetRevision: "revision", RepoURL: "repoURL"},
					Destination: c.destinationField,
				},
			}

			initObjs := []crtclient.Object{&app, &appSet}

			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(initObjs...).Build()
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-secret",
					Namespace: "namespace",
					Labels: map[string]string{
						generators.ArgoCDSecretTypeLabel: generators.ArgoCDSecretTypeCluster,
					},
				},
				Data: map[string][]byte{
					// Since this test requires the cluster to be an invalid destination, we
					// always return a cluster named 'my-cluster2' (different from app 'my-cluster', above)
					"name":   []byte("mycluster2"),
					"server": []byte("https://kubernetes.default.svc"),
					"config": []byte("{\"username\":\"foo\",\"password\":\"foo\"}"),
				},
			}

			objects := append([]runtime.Object{}, secret)
			kubeclientset := kubefake.NewSimpleClientset(objects...)

			r := ApplicationSetReconciler{
				Client:        client,
				Scheme:        scheme,
				Recorder:      record.NewFakeRecorder(10),
				KubeClientset: kubeclientset,
			}

			clusterList, err := utils.ListClusters(context.Background(), kubeclientset, "namespace")
			assert.NoError(t, err, "Unexpected error")

			appLog := log.WithFields(log.Fields{"app": app.Name, "appSet": ""})

			appInputParam := app.DeepCopy()

			err = r.removeFinalizerOnInvalidDestination(context.Background(), appSet, appInputParam, clusterList, appLog)
			assert.NoError(t, err, "Unexpected error")

			retrievedApp := argov1alpha1.Application{}
			err = client.Get(context.Background(), crtclient.ObjectKeyFromObject(&app), &retrievedApp)
			assert.NoError(t, err, "Unexpected error")

			finalizerRemoved := len(retrievedApp.Finalizers) == 0

			assert.True(t, c.expectFinalizerRemoved == finalizerRemoved)

			bytes, _ := json.MarshalIndent(retrievedApp, "", "  ")
			t.Log("Contents of app after call:", string(bytes))

		})
	}
}

func TestCreateApplications(t *testing.T) {

	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	for _, c := range []struct {
		appSet     argoprojiov1alpha1.ApplicationSet
		existsApps []argov1alpha1.Application
		apps       []argov1alpha1.Application
		expected   []argov1alpha1.Application
	}{
		{
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
			},
			existsApps: nil,
			apps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "1",
					},
				},
			},
		},
		{
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			},
			existsApps: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "test",
					},
				},
			},
			apps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "test",
					},
				},
			},
		},
		{
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			},
			existsApps: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app1",
						Namespace:       "namespace",
						ResourceVersion: "2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "test",
					},
				},
			},
			apps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "app2",
						Namespace:       "namespace",
						ResourceVersion: "1",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
		},
	} {
		initObjs := []crtclient.Object{&c.appSet}
		for _, a := range c.existsApps {
			err = controllerutil.SetControllerReference(&c.appSet, &a, scheme)
			assert.Nil(t, err)
			initObjs = append(initObjs, &a)
		}

		client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(initObjs...).Build()

		r := ApplicationSetReconciler{
			Client:   client,
			Scheme:   scheme,
			Recorder: record.NewFakeRecorder(len(initObjs) + len(c.expected)),
		}

		err = r.createInCluster(context.TODO(), c.appSet, c.apps)
		assert.Nil(t, err)

		for _, obj := range c.expected {
			got := &argov1alpha1.Application{}
			_ = client.Get(context.Background(), crtclient.ObjectKey{
				Namespace: obj.Namespace,
				Name:      obj.Name,
			}, got)

			err = controllerutil.SetControllerReference(&c.appSet, &obj, r.Scheme)
			assert.Nil(t, err)

			assert.Equal(t, obj, *got)
		}
	}

}

func TestDeleteInCluster(t *testing.T) {

	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)
	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	for _, c := range []struct {
		// appSet is the application set on which the delete function is called
		appSet argoprojiov1alpha1.ApplicationSet
		// existingApps is the current state of Applications on the cluster
		existingApps []argov1alpha1.Application
		// desireApps is the apps generated by the generator that we wish to keep alive
		desiredApps []argov1alpha1.Application
		// expected is the list of applications that we expect to exist after calling delete
		expected []argov1alpha1.Application
		// notExpected is the list of applications that we expect not to exist after calling delete
		notExpected []argov1alpha1.Application
	}{
		{
			appSet: argoprojiov1alpha1.ApplicationSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "name",
					Namespace: "namespace",
				},
				Spec: argoprojiov1alpha1.ApplicationSetSpec{
					Template: argoprojiov1alpha1.ApplicationSetTemplate{
						Spec: argov1alpha1.ApplicationSpec{
							Project: "project",
						},
					},
				},
			},
			existingApps: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "delete",
						Namespace:       "namespace",
						ResourceVersion: "2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "keep",
						Namespace:       "namespace",
						ResourceVersion: "2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			desiredApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "keep",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			expected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "keep",
						Namespace:       "namespace",
						ResourceVersion: "2",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
			notExpected: []argov1alpha1.Application{
				{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Application",
						APIVersion: "argoproj.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            "delete",
						Namespace:       "namespace",
						ResourceVersion: "1",
					},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "project",
					},
				},
			},
		},
	} {
		initObjs := []crtclient.Object{&c.appSet}
		for _, a := range c.existingApps {
			temp := a
			err = controllerutil.SetControllerReference(&c.appSet, &temp, scheme)
			assert.Nil(t, err)
			initObjs = append(initObjs, &temp)
		}

		client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(initObjs...).Build()

		r := ApplicationSetReconciler{
			Client:        client,
			Scheme:        scheme,
			Recorder:      record.NewFakeRecorder(len(initObjs) + len(c.expected)),
			KubeClientset: kubefake.NewSimpleClientset(),
		}

		err = r.deleteInCluster(context.TODO(), c.appSet, c.desiredApps)
		assert.Nil(t, err)

		// For each of the expected objects, verify they exist on the cluster
		for _, obj := range c.expected {
			got := &argov1alpha1.Application{}
			_ = client.Get(context.Background(), crtclient.ObjectKey{
				Namespace: obj.Namespace,
				Name:      obj.Name,
			}, got)

			err = controllerutil.SetControllerReference(&c.appSet, &obj, r.Scheme)
			assert.Nil(t, err)

			assert.Equal(t, obj, *got)
		}

		// Verify each of the unexpected objs cannot be found
		for _, obj := range c.notExpected {
			got := &argov1alpha1.Application{}
			err := client.Get(context.Background(), crtclient.ObjectKey{
				Namespace: obj.Namespace,
				Name:      obj.Name,
			}, got)

			assert.EqualError(t, err, fmt.Sprintf("applications.argoproj.io \"%s\" not found", obj.Name))
		}
	}
}

func TestGetMinRequeueAfter(t *testing.T) {
	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)
	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	generator := argoprojiov1alpha1.ApplicationSetGenerator{
		List:     &argoprojiov1alpha1.ListGenerator{},
		Git:      &argoprojiov1alpha1.GitGenerator{},
		Clusters: &argoprojiov1alpha1.ClusterGenerator{},
	}

	generatorMock0 := generatorMock{}
	generatorMock0.On("GetRequeueAfter", &generator).
		Return(generators.NoRequeueAfter)

	generatorMock1 := generatorMock{}
	generatorMock1.On("GetRequeueAfter", &generator).
		Return(time.Duration(1) * time.Second)

	generatorMock10 := generatorMock{}
	generatorMock10.On("GetRequeueAfter", &generator).
		Return(time.Duration(10) * time.Second)

	r := ApplicationSetReconciler{
		Client:   client,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(0),
		Generators: map[string]generators.Generator{
			"List":     &generatorMock10,
			"Git":      &generatorMock1,
			"Clusters": &generatorMock1,
		},
	}

	got := r.getMinRequeueAfter(&argoprojiov1alpha1.ApplicationSet{
		Spec: argoprojiov1alpha1.ApplicationSetSpec{
			Generators: []argoprojiov1alpha1.ApplicationSetGenerator{generator},
		},
	})

	assert.Equal(t, time.Duration(1)*time.Second, got)
}
func TestHasDuplicateNames(t *testing.T) {

	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)
	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	for _, c := range []struct {
		testName      string
		desiredApps   []argov1alpha1.Application
		hasDuplicates bool
		duplicateName string
	}{
		{
			testName: "has no duplicates",
			desiredApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app2",
					},
				},
			},
			hasDuplicates: false,
			duplicateName: "",
		},
		{
			testName: "has duplicates",
			desiredApps: []argov1alpha1.Application{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app2",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "app1",
					},
				},
			},
			hasDuplicates: true,
			duplicateName: "app1",
		},
	} {
		hasDuplicates, name := hasDuplicateNames(c.desiredApps)
		assert.Equal(t, c.hasDuplicates, hasDuplicates)
		assert.Equal(t, c.duplicateName, name)
	}
}

func TestValidateGeneratedApplications(t *testing.T) {

	scheme := runtime.NewScheme()
	err := argoprojiov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	err = argov1alpha1.AddToScheme(scheme)
	assert.Nil(t, err)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	// Valid cluster
	myCluster := argov1alpha1.Cluster{
		Server: "https://kubernetes.default.svc",
		Name:   "my-cluster",
	}

	// Valid project
	myProject := &argov1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "namespace"},
		Spec: argov1alpha1.AppProjectSpec{
			SourceRepos: []string{"*"},
			Destinations: []argov1alpha1.ApplicationDestination{
				{
					Namespace: "*",
					Server:    "*",
				},
			},
			ClusterResourceWhitelist: []metav1.GroupKind{
				{
					Group: "*",
					Kind:  "*",
				},
			},
		},
	}

	// Test a subset of the validations that 'validateGeneratedApplications' performs
	for _, cc := range []struct {
		name           string
		apps           []argov1alpha1.Application
		expectedErrors []string
	}{
		{
			name: "valid app should return true",
			apps: []argov1alpha1.Application{
				{
					TypeMeta:   metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "default",
						Source: argov1alpha1.ApplicationSource{
							RepoURL:        "https://url",
							Path:           "/",
							TargetRevision: "HEAD",
						},
						Destination: argov1alpha1.ApplicationDestination{
							Namespace: "namespace",
							Name:      "my-cluster",
						},
					},
				},
			},
			expectedErrors: []string{},
		},
		{
			name: "can't have both name and server defined",
			apps: []argov1alpha1.Application{
				{
					TypeMeta:   metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "default",
						Source: argov1alpha1.ApplicationSource{
							RepoURL:        "https://url",
							Path:           "/",
							TargetRevision: "HEAD",
						},
						Destination: argov1alpha1.ApplicationDestination{
							Namespace: "namespace",
							Server:    "my-server",
							Name:      "my-cluster",
						},
					},
				},
			},
			expectedErrors: []string{"application destination can't have both name and server defined"},
		},
		{
			name: "project mismatch should return error",
			apps: []argov1alpha1.Application{
				{
					TypeMeta:   metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "DOES-NOT-EXIST",
						Source: argov1alpha1.ApplicationSource{
							RepoURL:        "https://url",
							Path:           "/",
							TargetRevision: "HEAD",
						},
						Destination: argov1alpha1.ApplicationDestination{
							Namespace: "namespace",
							Name:      "my-cluster",
						},
					},
				},
			},
			expectedErrors: []string{"application references project DOES-NOT-EXIST which does not exist"},
		},
		{
			name: "valid app should return true",
			apps: []argov1alpha1.Application{
				{
					TypeMeta:   metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "default",
						Source: argov1alpha1.ApplicationSource{
							RepoURL:        "https://url",
							Path:           "/",
							TargetRevision: "HEAD",
						},
						Destination: argov1alpha1.ApplicationDestination{
							Namespace: "namespace",
							Name:      "my-cluster",
						},
					},
				},
			},
			expectedErrors: []string{},
		},
		{
			name: "cluster should match",
			apps: []argov1alpha1.Application{
				{
					TypeMeta:   metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{},
					Spec: argov1alpha1.ApplicationSpec{
						Project: "default",
						Source: argov1alpha1.ApplicationSource{
							RepoURL:        "https://url",
							Path:           "/",
							TargetRevision: "HEAD",
						},
						Destination: argov1alpha1.ApplicationDestination{
							Namespace: "namespace",
							Name:      "nonexistent-cluster",
						},
					},
				},
			},
			expectedErrors: []string{"there are no clusters with this name: nonexistent-cluster"},
		},
	} {

		t.Run(cc.name, func(t *testing.T) {

			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-secret",
					Namespace: "namespace",
					Labels: map[string]string{
						generators.ArgoCDSecretTypeLabel: generators.ArgoCDSecretTypeCluster,
					},
				},
				Data: map[string][]byte{
					"name":   []byte("my-cluster"),
					"server": []byte("https://kubernetes.default.svc"),
					"config": []byte("{\"username\":\"foo\",\"password\":\"foo\"}"),
				},
			}

			objects := append([]runtime.Object{}, secret)
			kubeclientset := kubefake.NewSimpleClientset(objects...)

			argoDBMock := dbmocks.ArgoDB{}
			argoDBMock.On("GetCluster", mock.Anything, "https://kubernetes.default.svc").Return(&myCluster, nil)
			argoDBMock.On("ListClusters", mock.Anything).Return(&argov1alpha1.ClusterList{Items: []argov1alpha1.Cluster{
				myCluster,
			}}, nil)

			argoObjs := []runtime.Object{myProject}
			for _, app := range cc.apps {
				argoObjs = append(argoObjs, &app)
			}

			r := ApplicationSetReconciler{
				Client:           client,
				Scheme:           scheme,
				Recorder:         record.NewFakeRecorder(1),
				Generators:       map[string]generators.Generator{},
				ArgoDB:           &argoDBMock,
				ArgoAppClientset: appclientset.NewSimpleClientset(argoObjs...),
				KubeClientset:    kubeclientset,
			}

			appSetInfo := argoprojiov1alpha1.ApplicationSet{}

			err := r.validateGeneratedApplications(context.TODO(), cc.apps, appSetInfo, "namespace")

			if err == nil {
				assert.Equal(t, len(cc.expectedErrors), 0, "Expected errors but none were seen")
			} else {
				// An error was returned: it should be expected
				matched := false
				for _, expectedErr := range cc.expectedErrors {
					foundMatch := strings.Contains(err.Error(), expectedErr)
					assert.True(t, foundMatch, "Unble to locate expected error: %s", cc.expectedErrors)
					matched = matched || foundMatch
				}
				assert.True(t, matched, "An unexpected error occurrred: %v", err)
			}
		})
	}
}
