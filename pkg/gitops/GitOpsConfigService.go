/*
 * Copyright (c) 2020 Devtron Labs
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package gitops

import (
	"encoding/json"
	"fmt"
	"github.com/devtron-labs/devtron/client/argocdServer"
	"github.com/devtron-labs/devtron/internal/sql/models"
	"github.com/devtron-labs/devtron/internal/sql/repository"
	"github.com/devtron-labs/devtron/internal/util"
	"github.com/devtron-labs/devtron/pkg/cluster"
	"github.com/devtron-labs/devtron/pkg/pipeline"
	"github.com/devtron-labs/devtron/pkg/user"
	"github.com/ghodss/yaml"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/errors"
	"net/http"
	"time"
)

type GitOpsConfigService interface {
	CreateGitOpsConfig(config *GitOpsConfigDto) (*GitOpsConfigDto, error)
	UpdateGitOpsConfig(config *GitOpsConfigDto) error
	GetGitOpsConfigById(id int) (*GitOpsConfigDto, error)
	GetAllGitOpsConfig() ([]*GitOpsConfigDto, error)
	GetGitOpsConfigByProvider(provider string) (*GitOpsConfigDto, error)
}

type GitOpsConfigDto struct {
	Id            int    `json:"id,omitempty"`
	Provider      string `json:"provider"`
	Username      string `json:"username"`
	Token         string `json:"token"`
	GitLabGroupId string `json:"gitLabGroupId"`
	GitHubOrgId   string `json:"gitHubOrgId"`
	Host          string `json:"host"`
	Active        bool   `json:"active"`
	UserId        int32  `json:"-"`
}

const GitOpsSecretName = "devtron-gitops-secret"

type GitOpsConfigServiceImpl struct {
	logger           *zap.SugaredLogger
	gitOpsRepository repository.GitOpsConfigRepository
	K8sUtil          *util.K8sUtil
	aCDAuthConfig    *user.ACDAuthConfig
	clusterService   cluster.ClusterService
	envService       cluster.EnvironmentService
	versionService   argocdServer.VersionService
}

func NewGitOpsConfigServiceImpl(Logger *zap.SugaredLogger, ciHandler pipeline.CiHandler,
	gitOpsRepository repository.GitOpsConfigRepository, K8sUtil *util.K8sUtil, aCDAuthConfig *user.ACDAuthConfig,
	clusterService cluster.ClusterService, envService cluster.EnvironmentService, versionService argocdServer.VersionService) *GitOpsConfigServiceImpl {
	return &GitOpsConfigServiceImpl{
		logger:           Logger,
		gitOpsRepository: gitOpsRepository,
		K8sUtil:          K8sUtil,
		aCDAuthConfig:    aCDAuthConfig,
		clusterService:   clusterService,
		envService:       envService,
		versionService:   versionService,
	}
}
func (impl *GitOpsConfigServiceImpl) CreateGitOpsConfig(request *GitOpsConfigDto) (*GitOpsConfigDto, error) {
	impl.logger.Debugw("gitops create request", "req", request)
	model := &repository.GitOpsConfig{
		Provider:      request.Provider,
		Username:      request.Username,
		Token:         request.Token,
		GitHubOrgId:   request.GitHubOrgId,
		GitLabGroupId: request.GitLabGroupId,
		Host:          request.Host,
		Active:        request.Active,
		AuditLog:      models.AuditLog{CreatedBy: request.UserId, CreatedOn: time.Now(), UpdatedOn: time.Now(), UpdatedBy: request.UserId},
	}
	model, err := impl.gitOpsRepository.CreateGitOpsConfig(model)
	if err != nil {
		impl.logger.Errorw("error in saving gitops config", "data", model, "err", err)
		err = &util.ApiError{
			InternalMessage: "gitops config failed to create in db",
			UserMessage:     "gitops config failed to create in db",
		}
		return nil, err
	}

	clusterBean, err := impl.clusterService.FindOne(cluster.ClusterName)
	if err != nil {
		return nil, err
	}
	cfg, err := impl.envService.GetClusterConfig(clusterBean)
	if err != nil {
		return nil, err
	}

	client, err := impl.K8sUtil.GetClient(cfg)
	if err != nil {
		return nil, err
	}

	secret, err := impl.K8sUtil.GetSecretFast(impl.aCDAuthConfig.ACDConfigMapNamespace, GitOpsSecretName, client)
	statusError, _ := err.(*errors.StatusError)
	if err != nil && statusError.Status().Code != http.StatusNotFound {
		impl.logger.Errorw("secret not found", "err", err)
		return nil, err
	}
	if secret == nil {
		secret, err = impl.K8sUtil.CreateSecretFast(impl.aCDAuthConfig.ACDConfigMapNamespace, request.Username, request.Token, client)
		if err != nil {
			impl.logger.Errorw("err", "err", err)
			return nil, err
		}
	}

	operationComplete := false
	retryCount := 0
	for !operationComplete && retryCount < 3 {
		retryCount = retryCount + 1

		cm, err := impl.K8sUtil.GetConfigMapFast(impl.aCDAuthConfig.ACDConfigMapNamespace, impl.aCDAuthConfig.ACDConfigMapName, client)
		if err != nil {
			return nil, err
		}
		updatedData, existsInArgodCm := impl.updateData(cm.Data, request, GitOpsSecretName)
		if ! existsInArgodCm {
			data := cm.Data
			data["repository.credentials"] = updatedData["repository.credentials"]
			cm.Data = data
			_, err = impl.K8sUtil.UpdateConfigMapFast(impl.aCDAuthConfig.ACDConfigMapNamespace, cm, client)
			if err != nil {
				continue
			}
			if err == nil {
				operationComplete = true
			}
		} else {
			operationComplete = true
		}
	}
	if !operationComplete {
		return nil, fmt.Errorf("resouce version not matched with config map attemped 3 times")
	}

	request.Id = model.Id
	return request, nil
}
func (impl *GitOpsConfigServiceImpl) UpdateGitOpsConfig(request *GitOpsConfigDto) error {
	impl.logger.Debugw("gitops config update request", "req", request)
	model, err := impl.gitOpsRepository.GetGitOpsConfigById(request.Id)
	if err != nil {
		impl.logger.Errorw("No matching entry found for update.", "id", request.Id)
		err = &util.ApiError{
			InternalMessage: "gitops config update failed, does not exist",
			UserMessage:     "gitops config update failed, does not exist",
		}
		return err
	}
	model.Provider = request.Provider
	model.Username = request.Username
	model.Token = request.Token
	model.GitLabGroupId = request.GitLabGroupId
	model.GitHubOrgId = request.GitHubOrgId
	model.Host = request.Host
	model.Active = request.Active
	err = impl.gitOpsRepository.UpdateGitOpsConfig(model)
	if err != nil {
		impl.logger.Errorw("error in updating team", "data", model, "err", err)
		err = &util.ApiError{
			InternalMessage: "gitops config failed to update in db",
			UserMessage:     "gitops config failed to update in db",
		}
		return err
	}
	request.Id = model.Id

	clusterBean, err := impl.clusterService.FindOne(cluster.ClusterName)
	if err != nil {
		return err
	}
	cfg, err := impl.envService.GetClusterConfig(clusterBean)
	if err != nil {
		return err
	}

	client, err := impl.K8sUtil.GetClient(cfg)
	if err != nil {
		return err
	}

	secret, err := impl.K8sUtil.GetSecretFast(impl.aCDAuthConfig.ACDConfigMapNamespace, GitOpsSecretName, client)
	statusError, _ := err.(*errors.StatusError)
	if err != nil && statusError.Status().Code != http.StatusNotFound {
		impl.logger.Errorw("secret not found", "err", err)
		return err
	}
	if secret == nil {
		secret, err = impl.K8sUtil.CreateSecretFast(impl.aCDAuthConfig.ACDConfigMapNamespace, request.Username, request.Token, client)
		if err != nil {
			impl.logger.Errorw("err", "err", err)
			return err
		}
	}

	operationComplete := false
	retryCount := 0
	for !operationComplete && retryCount < 3 {
		retryCount = retryCount + 1

		cm, err := impl.K8sUtil.GetConfigMapFast(impl.aCDAuthConfig.ACDConfigMapNamespace, impl.aCDAuthConfig.ACDConfigMapName, client)
		if err != nil {
			return err
		}
		updatedData, existsInArgodCm := impl.updateData(cm.Data, request, GitOpsSecretName)
		if ! existsInArgodCm {
			data := cm.Data
			data["repository.credentials"] = updatedData["repository.credentials"]
			cm.Data = data
			_, err = impl.K8sUtil.UpdateConfigMapFast(impl.aCDAuthConfig.ACDConfigMapNamespace, cm, client)
			if err != nil {
				continue
			}
			if err == nil {
				operationComplete = true
			}
		} else {
			operationComplete = true
		}
	}
	if !operationComplete {
		return fmt.Errorf("resouce version not matched with config map attemped 3 times")
	}

	return nil
}

func (impl *GitOpsConfigServiceImpl) GetGitOpsConfigById(id int) (*GitOpsConfigDto, error) {
	model, err := impl.gitOpsRepository.GetGitOpsConfigById(id)
	if err != nil {
		impl.logger.Errorw("GetGitOpsConfigById, error while get by id", "err", err, "id", id)
		return nil, err
	}
	config := &GitOpsConfigDto{
		Id:            model.Id,
		Provider:      model.Provider,
		GitHubOrgId:   model.GitHubOrgId,
		GitLabGroupId: model.GitLabGroupId,
		Active:        model.Active,
		UserId:        model.CreatedBy,
	}

	return config, err
}

func (impl *GitOpsConfigServiceImpl) GetAllGitOpsConfig() ([]*GitOpsConfigDto, error) {
	models, err := impl.gitOpsRepository.GetAllGitOpsConfig()
	if err != nil {
		impl.logger.Errorw("GetAllGitOpsConfig, error while fetch all", "err", err)
		return nil, err
	}
	var configs []*GitOpsConfigDto
	for _, model := range models {
		config := &GitOpsConfigDto{
			Id:            model.Id,
			Provider:      model.Provider,
			GitHubOrgId:   model.GitHubOrgId,
			GitLabGroupId: model.GitLabGroupId,
			Active:        model.Active,
			UserId:        model.CreatedBy,
		}
		configs = append(configs, config)
	}
	return configs, err
}

func (impl *GitOpsConfigServiceImpl) GetGitOpsConfigByProvider(provider string) (*GitOpsConfigDto, error) {
	model, err := impl.gitOpsRepository.GetGitOpsConfigByProvider(provider)
	if err != nil {
		impl.logger.Errorw("GetGitOpsConfigByProvider, error while get by name", "err", err, "provider", provider)
		return nil, err
	}
	config := &GitOpsConfigDto{
		Id:            model.Id,
		Provider:      model.Provider,
		GitHubOrgId:   model.GitHubOrgId,
		GitLabGroupId: model.GitLabGroupId,
		Active:        model.Active,
		UserId:        model.CreatedBy,
	}

	return config, err
}

func (impl *GitOpsConfigServiceImpl) updateData(data map[string]string, request *GitOpsConfigDto, secretName string) (map[string]string, bool) {
	found := false
	var repositories []*RepositoryCredentialsDto
	repoStr := data["repository.credentials"]
	if len(repoStr) > 0 {
		repoByte, err := yaml.YAMLToJSON([]byte(repoStr))
		if err != nil {
			panic(err)
		}
		err = json.Unmarshal(repoByte, &repositories)
		if err != nil {
			panic(err)
		}
	}
	for _, item := range repositories {
		if item.Url == request.Host {
			usernameSecret := &KeyDto{Name: secretName, Key: "username"}
			passwordSecret := &KeyDto{Name: secretName, Key: "password"}
			item.PasswordSecret = passwordSecret
			item.UsernameSecret = usernameSecret
			found = true
		}
	}
	if !found {
		repoData := impl.createRepoElement(request)
		repositories = append(repositories, repoData)
	}
	rb, err := json.Marshal(repositories)
	if err != nil {
		panic(err)
	}
	repositoriesYamlByte, err := yaml.JSONToYAML(rb)
	if err != nil {
		panic(err)
	}
	repositoryCredentials := map[string]string{}
	if len(repositoriesYamlByte) > 0 {
		repositoryCredentials["repository.credentials"] = string(repositoriesYamlByte)
	}
	return repositoryCredentials, found
}

func (impl *GitOpsConfigServiceImpl) createRepoElement(request *GitOpsConfigDto) *RepositoryCredentialsDto {
	repoData := &RepositoryCredentialsDto{}
	usernameSecret := &KeyDto{Name: request.Username, Key: "username"}
	passwordSecret := &KeyDto{Name: request.Token, Key: "password"}
	repoData.PasswordSecret = passwordSecret
	repoData.UsernameSecret = usernameSecret
	repoData.Url = request.Host
	return repoData
}

type RepositoryCredentialsDto struct {
	Url            string  `json:"url,omitempty"`
	UsernameSecret *KeyDto `json:"usernameSecret,omitempty"`
	PasswordSecret *KeyDto `json:"passwordSecret,omitempty"`
}

type KeyDto struct {
	Name string `json:"name,omitempty"`
	Key  string `json:"key,omitempty"`
}