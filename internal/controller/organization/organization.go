/*
Copyright 2022 The Crossplane Authors.

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

package organization

import (
	"context"
	"reflect"
	"slices"
	"sort"

	"github.com/google/go-cmp/cmp"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/connection"
	"github.com/crossplane/crossplane-runtime/pkg/controller"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/crossplane/provider-github/internal/util"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/types"
	pointer "k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/provider-github/apis/organizations/v1alpha1"
	apisv1alpha1 "github.com/crossplane/provider-github/apis/v1alpha1"
	ghclient "github.com/crossplane/provider-github/internal/clients"
	"github.com/crossplane/provider-github/internal/features"

	"github.com/google/go-github/v62/github"
)

const (
	errNotOrganization = "managed resource is not a Organization custom resource"
	errTrackPCUsage    = "cannot track ProviderConfig usage"
	errGetPC           = "cannot get ProviderConfig"
	errGetCreds        = "cannot get credentials"

	errNewClient = "cannot create new Service"
)

// Setup adds a controller that reconciles Organization managed resources.
func Setup(mgr ctrl.Manager, o controller.Options) error {
	name := managed.ControllerName(v1alpha1.OrganizationGroupKind)

	cps := []managed.ConnectionPublisher{managed.NewAPISecretPublisher(mgr.GetClient(), mgr.GetScheme())}
	if o.Features.Enabled(features.EnableAlphaExternalSecretStores) {
		cps = append(cps, connection.NewDetailsManager(mgr.GetClient(), apisv1alpha1.StoreConfigGroupVersionKind))
	}

	r := managed.NewReconciler(mgr,
		resource.ManagedKind(v1alpha1.OrganizationGroupVersionKind),
		managed.WithExternalConnecter(&connector{
			kube:        mgr.GetClient(),
			usage:       resource.NewProviderConfigUsageTracker(mgr.GetClient(), &apisv1alpha1.ProviderConfigUsage{}),
			newClientFn: ghclient.NewClient}),
		managed.WithLogger(o.Logger.WithValues("controller", name)),
		managed.WithPollInterval(o.PollInterval),
		managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name))),
		managed.WithConnectionPublishers(cps...))

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ForControllerRuntime()).
		WithEventFilter(resource.DesiredStateChanged()).
		For(&v1alpha1.Organization{}).
		Complete(ratelimiter.NewReconciler(name, r, o.GlobalRateLimiter))
}

type connector struct {
	kube        client.Client
	usage       resource.Tracker
	newClientFn func(string) (*ghclient.Client, error)
}

// Initializes external client
func (c *connector) Connect(ctx context.Context, mg resource.Managed) (managed.ExternalClient, error) {
	cr, ok := mg.(*v1alpha1.Organization)
	if !ok {
		return nil, errors.New(errNotOrganization)
	}

	if err := c.usage.Track(ctx, mg); err != nil {
		return nil, errors.Wrap(err, errTrackPCUsage)
	}

	pc := &apisv1alpha1.ProviderConfig{}
	if err := c.kube.Get(ctx, types.NamespacedName{Name: cr.GetProviderConfigReference().Name}, pc); err != nil {
		return nil, errors.Wrap(err, errGetPC)
	}

	cd := pc.Spec.Credentials
	data, err := resource.CommonCredentialExtractor(ctx, cd.Source, c.kube, cd.CommonCredentialSelectors)
	if err != nil {
		return nil, errors.Wrap(err, errGetCreds)
	}

	gh, err := c.newClientFn(string(data))
	if err != nil {
		return nil, errors.Wrap(err, errNewClient)
	}

	return &external{github: gh}, nil
}

type external struct {
	// A 'client' used to connect to the external resource API. In practice this
	// would be something like an AWS SDK client.
	github *ghclient.Client
}

//nolint:gocyclo
func (c *external) Observe(ctx context.Context, mg resource.Managed) (managed.ExternalObservation, error) {
	cr, ok := mg.(*v1alpha1.Organization)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errNotOrganization)
	}

	name := meta.GetExternalName(cr)

	org, _, err := c.github.Organizations.Get(ctx, name)

	if ghclient.Is404(err) {
		return managed.ExternalObservation{
			ResourceExists: false,
		}, nil
	}

	if err != nil {
		return managed.ExternalObservation{}, err
	}

	notUpToDate := managed.ExternalObservation{
		ResourceExists:   true,
		ResourceUpToDate: false,
	}

	// To use this function, the organization permission policy for enabled_repositories must be configured to selected, otherwise you get error 409 Conflict
	if cr.Spec.ForProvider.Actions.EnabledRepos != nil {
		aResp, _, err := c.github.Actions.ListEnabledReposInOrg(ctx, name, &github.ListOptions{PerPage: 100})

		if err != nil {
			return managed.ExternalObservation{}, err
		}

		crARepos := getSortedEnabledReposFromCr(cr.Spec.ForProvider.Actions.EnabledRepos)
		aRepos := getSortedRepoNames(aResp.Repositories)

		if err != nil {
			return managed.ExternalObservation{}, err
		}
		if !reflect.DeepEqual(aRepos, crARepos) {
			return notUpToDate, nil
		}
	}

	if cr.Spec.ForProvider.Secrets != nil {
		if cr.Spec.ForProvider.Secrets.ActionsSecrets != nil {
			crActionsSecretsToConfig, err := getOrgSecretsMapFromCr(ctx, c.github, name, cr.Spec.ForProvider.Secrets.ActionsSecrets)
			if err != nil {
				return managed.ExternalObservation{}, err
			}
			ghActionsSecretsToConfig, err := getOrgSecretsWithConfig(ctx, c.github.Actions, name, cr.Spec.ForProvider.Secrets.ActionsSecrets)
			if err != nil {
				return managed.ExternalObservation{}, err
			}
			if !cmp.Equal(crActionsSecretsToConfig, ghActionsSecretsToConfig) {
				return notUpToDate, nil
			}
		}
		if cr.Spec.ForProvider.Secrets.DependabotSecrets != nil {
			crDependabotSecretsToConfig, err := getOrgSecretsMapFromCr(ctx, c.github, name, cr.Spec.ForProvider.Secrets.DependabotSecrets)
			if err != nil {
				return managed.ExternalObservation{}, err
			}
			ghDependabotSecretsToConfig, err := getOrgSecretsWithConfig(ctx, c.github.Dependabot, name, cr.Spec.ForProvider.Secrets.DependabotSecrets)
			if err != nil {
				return managed.ExternalObservation{}, err
			}
			if !cmp.Equal(crDependabotSecretsToConfig, ghDependabotSecretsToConfig) {
				return notUpToDate, nil
			}
		}
	}

	if cr.Spec.ForProvider.Description != pointer.Deref(org.Description, "") {
		return notUpToDate, nil
	}

	cr.SetConditions(xpv1.Available())

	return managed.ExternalObservation{
		ResourceExists:   true,
		ResourceUpToDate: true,
	}, nil
}

func (c *external) Create(ctx context.Context, mg resource.Managed) (managed.ExternalCreation, error) {
	_, ok := mg.(*v1alpha1.Organization)
	if !ok {
		return managed.ExternalCreation{}, errors.New(errNotOrganization)
	}

	return managed.ExternalCreation{}, errors.New("Creation of organizations not supported!")
}

//nolint:gocyclo
func (c *external) Update(ctx context.Context, mg resource.Managed) (managed.ExternalUpdate, error) {
	cr, ok := mg.(*v1alpha1.Organization)
	if !ok {
		return managed.ExternalUpdate{}, errors.New(errNotOrganization)
	}

	name := meta.GetExternalName(cr)
	gh := c.github
	req := &github.Organization{
		Description: &cr.Spec.ForProvider.Description,
	}

	_, _, err := gh.Organizations.Edit(ctx, name, req)
	if err != nil {
		return managed.ExternalUpdate{}, err
	}

	missingReposIds, toDeleteReposIds, err := getMissingAndToDeleteRepos(ctx, gh, name, cr)
	if err != nil {
		return managed.ExternalUpdate{}, err
	}
	if cr.Spec.ForProvider.Actions.EnabledRepos != nil {
		err = updateRepos(ctx, gh, name, missingReposIds, toDeleteReposIds)
		if err != nil {
			return managed.ExternalUpdate{}, err
		}
	}

	secrets := cr.Spec.ForProvider.Secrets
	if secrets != nil {
		if secrets.ActionsSecrets != nil {
			err = updateOrgSecrets(ctx, gh, name, cr.Spec.ForProvider.Secrets.ActionsSecrets, &ActionsSecretSetter{client: gh})
			if err != nil {
				return managed.ExternalUpdate{}, err
			}
		}
		if secrets.DependabotSecrets != nil {
			err = updateOrgSecrets(ctx, gh, name, cr.Spec.ForProvider.Secrets.DependabotSecrets, &DependabotSecretSetter{client: gh})
			if err != nil {
				return managed.ExternalUpdate{}, err
			}
		}
	}

	return managed.ExternalUpdate{}, nil
}

func (c *external) Delete(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*v1alpha1.Organization)
	if !ok {
		return errors.New(errNotOrganization)
	}
	cr.Status.SetConditions(xpv1.Deleting())

	return nil
}

func getSortedEnabledReposFromCr(repos []v1alpha1.ActionEnabledRepo) []string {
	crAEnabledRepos := make([]string, 0, len(repos))
	for _, repo := range repos {
		crAEnabledRepos = append(crAEnabledRepos, repo.Repo)
	}
	slices.Sort(crAEnabledRepos)
	return crAEnabledRepos
}

func getSortedRepoNames(repos []*github.Repository) []string {
	repoNames := make([]string, 0, len(repos))
	for _, repo := range repos {
		repoNames = append(repoNames, repo.GetName())
	}
	slices.Sort(repoNames)
	return repoNames
}

func getUpdateRepoIds(ctx context.Context, gh *ghclient.Client, org string, crRepos []string, aRepos []string) ([]int64, error) {
	var updateRepos []string
	for _, repo := range crRepos {
		// Check if the repository from CRD is not in GitHub
		if !util.Contains(aRepos, repo) {
			updateRepos = append(updateRepos, repo)
		}
	}
	reposIds := make([]int64, 0, len(updateRepos))
	for _, repo := range updateRepos {
		repo, _, err := gh.Repositories.Get(ctx, org, repo)
		repoID := repo.GetID()
		reposIds = append(reposIds, repoID)
		if err != nil {
			return nil, err
		}
	}
	return reposIds, nil
}

func getMissingAndToDeleteRepos(ctx context.Context, gh *ghclient.Client, name string, cr *v1alpha1.Organization) ([]int64, []int64, error) {
	crARepos := getSortedEnabledReposFromCr(cr.Spec.ForProvider.Actions.EnabledRepos)

	// To use this function, the organization permission policy for enabled_repositories must be configured to selected, otherwise you get error 409 Conflict
	aResp, _, err := gh.Actions.ListEnabledReposInOrg(ctx, name, &github.ListOptions{PerPage: 100})
	if err != nil {
		return nil, nil, err
	}

	// Extract repository names from the list
	aRepos := getSortedRepoNames(aResp.Repositories)

	missingReposIds, err := getUpdateRepoIds(ctx, gh, name, crARepos, aRepos)
	if err != nil {
		return nil, nil, err
	}

	toDeleteReposIds, err := getUpdateRepoIds(ctx, gh, name, aRepos, crARepos)
	if err != nil {
		return nil, nil, err
	}

	return missingReposIds, toDeleteReposIds, nil
}

func updateRepos(ctx context.Context, gh *ghclient.Client, name string, missingReposIds []int64, toDeleteReposIds []int64) error {
	if len(missingReposIds) > 0 {
		for _, missingRepo := range missingReposIds {
			_, err := gh.Actions.AddEnabledReposInOrg(ctx, name, missingRepo)
			if err != nil {
				return err
			}
		}
	}

	if len(toDeleteReposIds) > 0 {
		for _, toDeleteRepo := range toDeleteReposIds {
			_, err := gh.Actions.RemoveEnabledReposInOrg(ctx, name, toDeleteRepo)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func getOrgSecretsMapFromCr(ctx context.Context, gh *ghclient.Client, org string, secrets []v1alpha1.OrgSecret) (map[string][]int64, error) {
	crOrgSecretsToConfig := make(map[string][]int64, len(secrets))
	for _, secret := range secrets {
		repoIds := make([]int64, 0, len(secret.RepositoryAccessList))
		for _, selectedRepo := range secret.RepositoryAccessList {
			ghRepo, _, err := gh.Repositories.Get(ctx, org, selectedRepo.Repo)
			if err != nil {
				return nil, err
			}
			repoIds = append(repoIds, ghRepo.GetID())
		}
		sort.Slice(repoIds, func(i, j int) bool {
			return repoIds[i] < repoIds[j]
		})
		crOrgSecretsToConfig[secret.Name] = repoIds
	}
	return crOrgSecretsToConfig, nil
}

type OrgSecretGetter interface {
	GetOrgSecret(ctx context.Context, owner, secretName string) (*github.Secret, *github.Response, error)
	ListSelectedReposForOrgSecret(ctx context.Context, owner, secretName string, opts *github.ListOptions) (*github.SelectedReposList, *github.Response, error)
}

func getOrgSecretsWithConfig(ctx context.Context, c OrgSecretGetter, owner string, secrets []v1alpha1.OrgSecret) (map[string][]int64, error) {
	orgSecretsToConfig := make(map[string][]int64, len(secrets))
	for _, secret := range secrets {
		ghSecret, _, err := c.GetOrgSecret(ctx, owner, secret.Name)
		if err != nil {
			return nil, err
		}
		repoIds := make([]int64, 0)
		if ghSecret != nil && ghSecret.Visibility == "selected" {
			opts := &github.ListOptions{PerPage: 100}
			for {
				ghRepo, resp, err := c.ListSelectedReposForOrgSecret(ctx, owner, secret.Name, opts)
				if err != nil {
					return nil, err
				}
				for _, selectedRepo := range ghRepo.Repositories {
					repoIds = append(repoIds, selectedRepo.GetID())
				}
				if resp.NextPage == 0 {
					break
				}
				opts.Page = resp.NextPage
			}
			sort.Slice(repoIds, func(i, j int) bool {
				return repoIds[i] < repoIds[j]
			})
		}
		orgSecretsToConfig[secret.Name] = repoIds
	}
	return orgSecretsToConfig, nil
}

type OrgSecretSetter interface {
	SetSelectedReposForOrgSecret(ctx context.Context, org string, name string, ids []int64) error
}

type ActionsSecretSetter struct {
	client *ghclient.Client
}

type DependabotSecretSetter struct {
	client *ghclient.Client
}

func (a *ActionsSecretSetter) SetSelectedReposForOrgSecret(ctx context.Context, org string, name string, ids []int64) error {
	_, err := a.client.Actions.SetSelectedReposForOrgSecret(ctx, org, name, ids)
	if err != nil {
		return err
	}
	return nil
}

func (d *DependabotSecretSetter) SetSelectedReposForOrgSecret(ctx context.Context, org string, name string, ids []int64) error {
	_, err := d.client.Dependabot.SetSelectedReposForOrgSecret(ctx, org, name, ids)
	if err != nil {
		return err
	}
	return nil
}

func updateOrgSecrets(ctx context.Context, gh *ghclient.Client, owner string, secrets []v1alpha1.OrgSecret, setter OrgSecretSetter) error {
	for _, secret := range secrets {
		repoIds := make([]int64, 0, len(secret.RepositoryAccessList))
		for _, repo := range secret.RepositoryAccessList {
			ghRepo, _, err := gh.Repositories.Get(ctx, owner, repo.Repo)
			if err != nil {
				return err
			}
			repoIds = append(repoIds, ghRepo.GetID())
		}
		err := setter.SetSelectedReposForOrgSecret(ctx, owner, secret.Name, repoIds)
		if err != nil {
			return err
		}
	}
	return nil
}
