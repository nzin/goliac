package internal

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/Alayacare/goliac/internal/config"
	"github.com/Alayacare/goliac/internal/entity"
	"github.com/Alayacare/goliac/internal/notification"
	"github.com/Alayacare/goliac/swagger_gen/models"
	"github.com/Alayacare/goliac/swagger_gen/restapi"
	"github.com/Alayacare/goliac/swagger_gen/restapi/operations"
	"github.com/Alayacare/goliac/swagger_gen/restapi/operations/app"
	"github.com/Alayacare/goliac/swagger_gen/restapi/operations/health"
	"github.com/go-openapi/loads"
	"github.com/go-openapi/runtime/middleware"
	"github.com/sirupsen/logrus"
)

/*
 * GoliacServer is here to run as a serve that
 * - sync/reconciliate periodically
 * - provide a REST API server
 */
type GoliacServer interface {
	Serve()
	GetLiveness(health.GetLivenessParams) middleware.Responder
	GetReadiness(health.GetReadinessParams) middleware.Responder
	PostFlushCache(app.PostFlushCacheParams) middleware.Responder
	PostResync(app.PostResyncParams) middleware.Responder
	GetStatus(app.GetStatusParams) middleware.Responder

	GetUsers(app.GetUsersParams) middleware.Responder
	GetUser(app.GetUserParams) middleware.Responder
	GetCollaborators(app.GetCollaboratorsParams) middleware.Responder
	GetCollaborator(app.GetCollaboratorParams) middleware.Responder
	GetTeams(app.GetTeamsParams) middleware.Responder
	GetTeam(app.GetTeamParams) middleware.Responder
	GetRepositories(app.GetRepositoriesParams) middleware.Responder
	GetRepository(app.GetRepositoryParams) middleware.Responder
}

type GoliacServerImpl struct {
	goliac              Goliac
	applyLobbyMutex     sync.Mutex
	applyLobbyCond      *sync.Cond
	applyCurrent        bool
	applyLobby          bool
	ready               bool // when the server has finished to load the local configuration
	lastSyncTime        *time.Time
	lastSyncError       error
	syncInterval        int64 // in seconds time remaining between 2 sync
	notificationService notification.NotificationService
}

func NewGoliacServer(goliac Goliac, notificationService notification.NotificationService) GoliacServer {

	server := GoliacServerImpl{
		goliac:              goliac,
		ready:               false,
		notificationService: notificationService,
	}
	server.applyLobbyCond = sync.NewCond(&server.applyLobbyMutex)

	return &server
}

func (g *GoliacServerImpl) GetRepositories(app.GetRepositoriesParams) middleware.Responder {
	local := g.goliac.GetLocal()
	repositories := make(models.Repositories, 0, len(local.Repositories()))

	for _, r := range local.Repositories() {
		repo := models.Repository{
			Name:     r.Name,
			Public:   r.Spec.IsPublic,
			Archived: r.Archived,
		}
		repositories = append(repositories, &repo)
	}

	return app.NewGetRepositoriesOK().WithPayload(repositories)
}

func (g *GoliacServerImpl) GetRepository(params app.GetRepositoryParams) middleware.Responder {
	local := g.goliac.GetLocal()

	repository, found := local.Repositories()[params.RepositoryID]
	if !found {
		message := fmt.Sprintf("Repository %s not found", params.RepositoryID)
		return app.NewGetRepositoryDefault(404).WithPayload(&models.Error{Message: &message})
	}

	teams := make([]*models.RepositoryDetailsTeamsItems0, 0)
	collaborators := make([]*models.RepositoryDetailsCollaboratorsItems0, 0)

	for _, r := range repository.Spec.Readers {
		team := models.RepositoryDetailsTeamsItems0{
			Name:   r,
			Access: "read",
		}
		teams = append(teams, &team)
	}

	if repository.Owner != nil {
		team := models.RepositoryDetailsTeamsItems0{
			Name:   *repository.Owner,
			Access: "write",
		}
		teams = append(teams, &team)
	}

	for _, w := range repository.Spec.Writers {
		team := models.RepositoryDetailsTeamsItems0{
			Name:   w,
			Access: "write",
		}
		teams = append(teams, &team)
	}

	for _, r := range repository.Spec.ExternalUserReaders {
		collaborator := models.RepositoryDetailsCollaboratorsItems0{
			Name:   r,
			Access: "read",
		}
		collaborators = append(collaborators, &collaborator)
	}

	for _, r := range repository.Spec.ExternalUserWriters {
		collaborator := models.RepositoryDetailsCollaboratorsItems0{
			Name:   r,
			Access: "write",
		}
		collaborators = append(collaborators, &collaborator)
	}

	repositoryDetails := models.RepositoryDetails{
		Name:                repository.Name,
		Public:              repository.Spec.IsPublic,
		AutoMergeAllowed:    repository.Spec.AllowAutoMerge,
		DeleteBranchOnMerge: repository.Spec.DeleteBranchOnMerge,
		AllowUpdateBranch:   repository.Spec.AllowUpdateBranch,
		Archived:            repository.Archived,
		Teams:               teams,
		Collaborators:       collaborators,
	}

	return app.NewGetRepositoryOK().WithPayload(&repositoryDetails)
}

func (g *GoliacServerImpl) GetTeams(app.GetTeamsParams) middleware.Responder {
	teams := make(models.Teams, 0)

	local := g.goliac.GetLocal()
	for teamname, team := range local.Teams() {
		t := models.Team{
			Name:    teamname,
			Members: team.Spec.Members,
			Owners:  team.Spec.Owners,
		}
		teams = append(teams, &t)

	}
	return app.NewGetTeamsOK().WithPayload(teams)
}

func (g *GoliacServerImpl) GetTeam(params app.GetTeamParams) middleware.Responder {
	local := g.goliac.GetLocal()

	team, found := local.Teams()[params.TeamID]
	if !found {
		message := fmt.Sprintf("Team %s not found", params.TeamID)
		return app.NewGetTeamDefault(404).WithPayload(&models.Error{Message: &message})
	}

	repos := make(map[string]*entity.Repository)
	for reponame, repo := range local.Repositories() {
		if repo.Owner != nil && *repo.Owner == params.TeamID {
			repos[reponame] = repo
		}
		for _, r := range repo.Spec.Readers {
			if r == params.TeamID {
				repos[reponame] = repo
				break
			}
		}
		for _, r := range repo.Spec.Writers {
			if r == params.TeamID {
				repos[reponame] = repo
				break
			}
		}
	}

	repositories := make([]*models.Repository, 0, len(repos))
	for reponame, repo := range repos {
		r := models.Repository{
			Name:                reponame,
			Archived:            repo.Archived,
			Public:              repo.Spec.IsPublic,
			AutoMergeAllowed:    repo.Spec.AllowAutoMerge,
			DeleteBranchOnMerge: repo.Spec.DeleteBranchOnMerge,
			AllowUpdateBranch:   repo.Spec.AllowUpdateBranch,
		}
		repositories = append(repositories, &r)
	}

	teamDetails := models.TeamDetails{
		Owners:       make([]*models.TeamDetailsOwnersItems0, len(team.Spec.Owners)),
		Members:      make([]*models.TeamDetailsMembersItems0, len(team.Spec.Members)),
		Name:         team.Name,
		Repositories: repositories,
	}

	for i, u := range team.Spec.Owners {
		if orgUser, ok := local.Users()[u]; ok {
			teamDetails.Owners[i] = &models.TeamDetailsOwnersItems0{
				Name:     u,
				Githubid: orgUser.Spec.GithubID,
				External: false,
			}
		} else {
			extUser := local.ExternalUsers()[u]
			teamDetails.Owners[i] = &models.TeamDetailsOwnersItems0{
				Name:     u,
				Githubid: extUser.Spec.GithubID,
				External: false,
			}
		}
	}

	for i, u := range team.Spec.Members {
		if orgUser, ok := local.Users()[u]; ok {
			teamDetails.Members[i] = &models.TeamDetailsMembersItems0{
				Name:     u,
				Githubid: orgUser.Spec.GithubID,
				External: false,
			}
		} else {
			extUser := local.ExternalUsers()[u]
			teamDetails.Members[i] = &models.TeamDetailsMembersItems0{
				Name:     u,
				Githubid: extUser.Spec.GithubID,
				External: false,
			}
		}

	}

	return app.NewGetTeamOK().WithPayload(&teamDetails)
}

func (g *GoliacServerImpl) GetCollaborators(app.GetCollaboratorsParams) middleware.Responder {
	users := make(models.Users, 0)

	local := g.goliac.GetLocal()
	for username, user := range local.ExternalUsers() {
		u := models.User{
			Name:     username,
			Githubid: user.Spec.GithubID,
		}
		users = append(users, &u)
	}
	return app.NewGetCollaboratorsOK().WithPayload(users)

}

func (g *GoliacServerImpl) GetCollaborator(params app.GetCollaboratorParams) middleware.Responder {
	local := g.goliac.GetLocal()

	user, found := local.ExternalUsers()[params.CollaboratorID]
	if !found {
		message := fmt.Sprintf("Collaborator %s not found", params.CollaboratorID)
		return app.NewGetCollaboratorDefault(404).WithPayload(&models.Error{Message: &message})
	}

	collaboratordetails := models.CollaboratorDetails{
		Githubid:     user.Spec.GithubID,
		Repositories: make([]*models.Repository, 0),
	}

	githubidToExternal := make(map[string]string)
	for _, e := range local.ExternalUsers() {
		githubidToExternal[e.Spec.GithubID] = e.Name
	}

	// let's sort repo per team
	for _, repo := range local.Repositories() {
		for _, r := range repo.Spec.ExternalUserReaders {
			if r == params.CollaboratorID {
				collaboratordetails.Repositories = append(collaboratordetails.Repositories, &models.Repository{
					Name:     repo.Name,
					Public:   repo.Spec.IsPublic,
					Archived: repo.Archived,
				})
			}
		}
		for _, r := range repo.Spec.ExternalUserWriters {
			if r == params.CollaboratorID {
				collaboratordetails.Repositories = append(collaboratordetails.Repositories, &models.Repository{
					Name:     repo.Name,
					Public:   repo.Spec.IsPublic,
					Archived: repo.Archived,
				})
			}
		}
	}

	return app.NewGetCollaboratorOK().WithPayload(&collaboratordetails)
}

func (g *GoliacServerImpl) GetUsers(app.GetUsersParams) middleware.Responder {
	users := make(models.Users, 0)

	local := g.goliac.GetLocal()
	for username, user := range local.Users() {
		u := models.User{
			Name:     username,
			Githubid: user.Spec.GithubID,
		}
		users = append(users, &u)
	}
	return app.NewGetUsersOK().WithPayload(users)
}

func (g *GoliacServerImpl) GetUser(params app.GetUserParams) middleware.Responder {
	local := g.goliac.GetLocal()

	user, found := local.Users()[params.UserID]
	if !found {
		message := fmt.Sprintf("User %s not found", params.UserID)
		return app.NewGetUserDefault(404).WithPayload(&models.Error{Message: &message})
	}

	userdetails := models.UserDetails{
		Githubid:     user.Spec.GithubID,
		Teams:        make([]*models.Team, 0),
		Repositories: make([]*models.Repository, 0),
	}

	// [teamname]team
	userTeams := make(map[string]*models.Team)
	for teamname, team := range local.Teams() {
		for _, owner := range team.Spec.Owners {
			if owner == params.UserID {
				team := models.Team{
					Name:    teamname,
					Members: team.Spec.Members,
					Owners:  team.Spec.Owners,
				}
				userTeams[teamname] = &team
				break
			}
		}
		for _, member := range team.Spec.Members {
			if member == params.UserID {
				team := models.Team{
					Name:    teamname,
					Members: team.Spec.Members,
					Owners:  team.Spec.Owners,
				}
				userTeams[teamname] = &team
				break
			}
		}
	}

	for _, t := range userTeams {
		userdetails.Teams = append(userdetails.Teams, t)
	}

	// let's sort repo per team
	teamRepo := make(map[string]map[string]*entity.Repository)
	for _, repo := range local.Repositories() {
		if repo.Owner != nil {
			if _, ok := teamRepo[*repo.Owner]; !ok {
				teamRepo[*repo.Owner] = make(map[string]*entity.Repository)
			}
			teamRepo[*repo.Owner][repo.Name] = repo
		}
		for _, r := range repo.Spec.Readers {
			if _, ok := teamRepo[r]; !ok {
				teamRepo[r] = make(map[string]*entity.Repository)
			}
			teamRepo[r][repo.Name] = repo
		}
		for _, w := range repo.Spec.Writers {
			if _, ok := teamRepo[w]; !ok {
				teamRepo[w] = make(map[string]*entity.Repository)
			}
			teamRepo[w][repo.Name] = repo
		}
	}

	// [reponame]repo
	userRepos := make(map[string]*entity.Repository)
	for _, team := range userdetails.Teams {
		if repositories, ok := teamRepo[team.Name]; ok {
			for n, r := range repositories {
				userRepos[n] = r
			}
		}
	}

	for _, r := range userRepos {
		repo := models.Repository{
			Name:     r.Name,
			Public:   r.Spec.IsPublic,
			Archived: r.Archived,
		}
		userdetails.Repositories = append(userdetails.Repositories, &repo)
	}

	return app.NewGetUserOK().WithPayload(&userdetails)
}

func (g *GoliacServerImpl) GetStatus(app.GetStatusParams) middleware.Responder {
	s := models.Status{
		LastSyncError:   "",
		LastSyncTime:    "N/A",
		NbRepos:         int64(len(g.goliac.GetLocal().Repositories())),
		NbTeams:         int64(len(g.goliac.GetLocal().Teams())),
		NbUsers:         int64(len(g.goliac.GetLocal().Users())),
		NbUsersExternal: int64(len(g.goliac.GetLocal().ExternalUsers())),
		Version:         config.GoliacBuildVersion,
	}
	if g.lastSyncError != nil {
		s.LastSyncError = g.lastSyncError.Error()
	}
	if g.lastSyncTime != nil {
		s.LastSyncTime = g.lastSyncTime.UTC().Format("2006-01-02T15:04:05")
	}
	return app.NewGetStatusOK().WithPayload(&s)
}

func (g *GoliacServerImpl) GetLiveness(params health.GetLivenessParams) middleware.Responder {
	return health.NewGetLivenessOK().WithPayload(&models.Health{Status: "OK"})
}

func (g *GoliacServerImpl) GetReadiness(params health.GetReadinessParams) middleware.Responder {
	if g.ready {
		return health.NewGetLivenessOK().WithPayload(&models.Health{Status: "OK"})
	} else {
		message := "Not yet ready, loading local state"
		return health.NewGetLivenessDefault(503).WithPayload(&models.Error{Message: &message})
	}
}

func (g *GoliacServerImpl) PostFlushCache(app.PostFlushCacheParams) middleware.Responder {
	g.goliac.FlushCache()
	return app.NewPostFlushCacheOK()
}

func (g *GoliacServerImpl) PostResync(app.PostResyncParams) middleware.Responder {
	go func() {
		g.triggerApply(true)
	}()
	return app.NewPostResyncOK()
}

func (g *GoliacServerImpl) Serve() {
	var wg sync.WaitGroup
	stopCh := make(chan struct{})

	restserver, err := g.StartRESTApi()
	if err != nil {
		logrus.Fatal(err)
	}

	// start the REST server
	go func() {
		if err := restserver.Serve(); err != nil {
			logrus.Error(err)
			close(stopCh)
		}
	}()

	// start the webhook server
	if config.Config.GithubWebhookDedicatedPort == config.Config.SwaggerPort {
		logrus.Warn("Github webhook server port is the same as the Swagger port, the webhook server will not be started")
	}

	var webhookserver GithubWebhookServer
	if config.Config.GithubWebhookDedicatedHost != "" &&
		config.Config.GithubWebhookDedicatedPort != 0 &&
		config.Config.GithubWebhookPath != "" &&
		config.Config.GithubWebhookSecret != "" &&
		config.Config.GithubWebhookDedicatedPort != config.Config.SwaggerPort {
		webhookserver = NewGithubWebhookServerImpl(
			config.Config.GithubWebhookDedicatedHost,
			config.Config.GithubWebhookDedicatedPort,
			config.Config.GithubWebhookPath,
			config.Config.GithubWebhookSecret,
			config.Config.ServerGitBranch, func() {
				g.triggerApply(false)
			},
		)
		go func() {
			if err := webhookserver.Start(); err != nil {
				logrus.Fatal(err)
				close(stopCh)
			}
		}()
	}

	logrus.Info("Server started")
	// Start the goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.syncInterval = 0
		for {
			select {
			case <-stopCh:
				restserver.Shutdown()
				if webhookserver != nil {
					webhookserver.Shutdown()
				}
				return
			default:
				g.syncInterval--
				time.Sleep(1 * time.Second)
				if g.syncInterval <= 0 {
					// we want to sync
					g.triggerApply(false)
				}
			}
		}
	}()

	// Handle OS signals
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)
	<-signalCh
	logrus.Info("Received OS signal, stopping Goliac...")

	close(stopCh)
	wg.Wait()
}

/*
 * triggerApply will trigger the apply process (by calling serveApply())
 * inside serverApply, it will check if the lobby is free
 * - if the lobby is free, it will start the apply process
 * - if the lobby is busy, it will do nothing
 */
func (g *GoliacServerImpl) triggerApply(forceresync bool) {
	err, applied := g.serveApply(forceresync)
	if !applied && err == nil {
		// the run was skipped
		g.syncInterval = config.Config.ServerApplyInterval
	} else {
		now := time.Now()
		g.lastSyncTime = &now
		previousError := g.lastSyncError
		g.lastSyncError = err
		// log the error only if it's a new one
		if err != nil && (previousError == nil || err.Error() != previousError.Error()) {
			logrus.Error(err)
			if err := g.notificationService.SendNotification(fmt.Sprintf("Goliac error when syncing: %s", err)); err != nil {
				logrus.Error(err)
			}
		}
		g.syncInterval = config.Config.ServerApplyInterval
	}
}

func (g *GoliacServerImpl) StartRESTApi() (*restapi.Server, error) {
	swaggerSpec, err := loads.Embedded(restapi.SwaggerJSON, restapi.FlatSwaggerJSON)
	if err != nil {
		return nil, err
	}

	api := operations.NewGoliacAPI(swaggerSpec)

	// configure API

	// healthcheck
	api.HealthGetLivenessHandler = health.GetLivenessHandlerFunc(g.GetLiveness)
	api.HealthGetReadinessHandler = health.GetReadinessHandlerFunc(g.GetReadiness)

	api.AppPostFlushCacheHandler = app.PostFlushCacheHandlerFunc(g.PostFlushCache)
	api.AppPostResyncHandler = app.PostResyncHandlerFunc(g.PostResync)
	api.AppGetStatusHandler = app.GetStatusHandlerFunc(g.GetStatus)

	api.AppGetUsersHandler = app.GetUsersHandlerFunc(g.GetUsers)
	api.AppGetUserHandler = app.GetUserHandlerFunc(g.GetUser)
	api.AppGetCollaboratorsHandler = app.GetCollaboratorsHandlerFunc(g.GetCollaborators)
	api.AppGetCollaboratorHandler = app.GetCollaboratorHandlerFunc(g.GetCollaborator)
	api.AppGetTeamsHandler = app.GetTeamsHandlerFunc(g.GetTeams)
	api.AppGetTeamHandler = app.GetTeamHandlerFunc(g.GetTeam)
	api.AppGetRepositoriesHandler = app.GetRepositoriesHandlerFunc(g.GetRepositories)
	api.AppGetRepositoryHandler = app.GetRepositoryHandlerFunc(g.GetRepository)

	server := restapi.NewServer(api)

	server.Host = config.Config.SwaggerHost
	server.Port = config.Config.SwaggerPort

	server.ConfigureAPI()

	return server, nil
}

func (g *GoliacServerImpl) serveApply(forceresync bool) (error, bool) {
	// we want to run ApplyToGithub
	// and queue one new run (the lobby) if a new run is asked
	g.applyLobbyMutex.Lock()
	// we already have a current run, and another waiting in the lobby
	if g.applyLobby {
		g.applyLobbyMutex.Unlock()
		return nil, false
	}

	if !g.applyCurrent {
		g.applyCurrent = true
	} else {
		g.applyLobby = true
		for g.applyLobby {
			g.applyLobbyCond.Wait()
		}
	}
	g.applyLobbyMutex.Unlock()

	// free the lobbdy (or just the current run) for the next run
	defer func() {
		g.applyLobbyMutex.Lock()
		if g.applyLobby {
			g.applyLobby = false
			g.applyLobbyCond.Signal()
		} else {
			g.applyCurrent = false
		}
		g.applyLobbyMutex.Unlock()
	}()

	repo := config.Config.ServerGitRepository
	branch := config.Config.ServerGitBranch

	if repo == "" {
		return fmt.Errorf("GOLIAC_SERVER_GIT_REPOSITORY env variable not set"), false
	}
	if branch == "" {
		return fmt.Errorf("GOLIAC_SERVER_GIT_BRANCH env variable not set"), false
	}

	// we are ready (to give local state, and to sync with remote)
	g.ready = true

	err := g.goliac.Apply(false, repo, branch, forceresync)
	if err != nil {
		return fmt.Errorf("failed to apply on branch %s: %s", branch, err), false
	}
	return nil, true
}
