package internal

import (
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Alayacare/goliac/internal/config"
	"github.com/Alayacare/goliac/internal/entity"
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
	goliac          Goliac
	applyLobbyMutex sync.Mutex
	applyLobbyCond  *sync.Cond
	applyCurrent    bool
	applyLobby      bool
	ready           bool // when the server has finished to load the local configuration
	lastSyncTime    *time.Time
	lastSyncError   error
	syncInterval    int // in seconds time remaining between 2 sync
}

func NewGoliacServer(goliac Goliac) GoliacServer {
	server := GoliacServerImpl{
		goliac: goliac,
		ready:  false,
	}
	server.applyLobbyCond = sync.NewCond(&server.applyLobbyMutex)

	return &server
}

func (g *GoliacServerImpl) GetRepositories(app.GetRepositoriesParams) middleware.Responder {
	local := g.goliac.GetLocal()
	repositories := make(models.Repositories, 0, len(local.Repositories()))

	for _, r := range local.Repositories() {
		repo := models.Repository{
			Name:     r.Metadata.Name,
			Public:   r.Data.IsPublic,
			Archived: r.Data.IsArchived,
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

	for _, r := range repository.Data.Readers {
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

	for _, w := range repository.Data.Writers {
		team := models.RepositoryDetailsTeamsItems0{
			Name:   w,
			Access: "write",
		}
		teams = append(teams, &team)
	}

	for _, r := range repository.Data.ExternalUserReaders {
		collaborator := models.RepositoryDetailsCollaboratorsItems0{
			Name:   r,
			Access: "read",
		}
		collaborators = append(collaborators, &collaborator)
	}

	for _, r := range repository.Data.ExternalUserWriters {
		collaborator := models.RepositoryDetailsCollaboratorsItems0{
			Name:   r,
			Access: "write",
		}
		collaborators = append(collaborators, &collaborator)
	}

	repositoryDetails := models.RepositoryDetails{
		Name:          repository.Metadata.Name,
		Public:        repository.Data.IsPublic,
		Archived:      repository.Data.IsArchived,
		Teams:         teams,
		Collaborators: collaborators,
	}

	return app.NewGetRepositoryOK().WithPayload(&repositoryDetails)
}

func (g *GoliacServerImpl) GetTeams(app.GetTeamsParams) middleware.Responder {
	teams := make(models.Teams, 0)

	local := g.goliac.GetLocal()
	for teamname, team := range local.Teams() {
		t := models.Team{
			Name:    teamname,
			Members: team.Data.Members,
			Owners:  team.Data.Owners,
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
		for _, r := range repo.Data.Readers {
			if r == params.TeamID {
				repos[reponame] = repo
				break
			}
		}
		for _, r := range repo.Data.Writers {
			if r == params.TeamID {
				repos[reponame] = repo
				break
			}
		}
	}

	repositories := make([]*models.Repository, 0, len(repos))
	for reponame, repo := range repos {
		r := models.Repository{
			Name:     reponame,
			Archived: repo.Data.IsArchived,
			Public:   repo.Data.IsPublic,
		}
		repositories = append(repositories, &r)
	}

	teamDetails := models.TeamDetails{
		Owners:       make([]*models.TeamDetailsOwnersItems0, len(team.Data.Owners)),
		Members:      make([]*models.TeamDetailsMembersItems0, len(team.Data.Members)),
		Name:         team.Metadata.Name,
		Repositories: repositories,
	}

	for i, u := range team.Data.Owners {
		if orgUser, ok := local.Users()[u]; ok {
			teamDetails.Owners[i] = &models.TeamDetailsOwnersItems0{
				Name:     u,
				Githubid: orgUser.Data.GithubID,
				External: false,
			}
		} else {
			extUser := local.ExternalUsers()[u]
			teamDetails.Owners[i] = &models.TeamDetailsOwnersItems0{
				Name:     u,
				Githubid: extUser.Data.GithubID,
				External: false,
			}
		}
	}

	for i, u := range team.Data.Members {
		if orgUser, ok := local.Users()[u]; ok {
			teamDetails.Members[i] = &models.TeamDetailsMembersItems0{
				Name:     u,
				Githubid: orgUser.Data.GithubID,
				External: false,
			}
		} else {
			extUser := local.ExternalUsers()[u]
			teamDetails.Members[i] = &models.TeamDetailsMembersItems0{
				Name:     u,
				Githubid: extUser.Data.GithubID,
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
			Githubid: user.Data.GithubID,
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
		Githubid:     user.Data.GithubID,
		Repositories: make([]*models.Repository, 0),
	}

	githubidToExternal := make(map[string]string)
	for _, e := range local.ExternalUsers() {
		githubidToExternal[e.Data.GithubID] = e.Metadata.Name
	}

	// let's sort repo per team
	for _, repo := range local.Repositories() {
		for _, r := range repo.Data.ExternalUserReaders {
			if r == params.CollaboratorID {
				collaboratordetails.Repositories = append(collaboratordetails.Repositories, &models.Repository{
					Name:     repo.Metadata.Name,
					Public:   repo.Data.IsPublic,
					Archived: repo.Data.IsArchived,
				})
			}
		}
		for _, r := range repo.Data.ExternalUserWriters {
			if r == params.CollaboratorID {
				collaboratordetails.Repositories = append(collaboratordetails.Repositories, &models.Repository{
					Name:     repo.Metadata.Name,
					Public:   repo.Data.IsPublic,
					Archived: repo.Data.IsArchived,
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
			Githubid: user.Data.GithubID,
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
		Githubid:     user.Data.GithubID,
		Teams:        make([]*models.Team, 0),
		Repositories: make([]*models.Repository, 0),
	}

	// [teamname]team
	userTeams := make(map[string]*models.Team)
	for teamname, team := range local.Teams() {
		for _, owner := range team.Data.Owners {
			if owner == params.UserID {
				team := models.Team{
					Name:    teamname,
					Members: team.Data.Members,
					Owners:  team.Data.Owners,
				}
				userTeams[teamname] = &team
				break
			}
		}
		for _, member := range team.Data.Members {
			if member == params.UserID {
				team := models.Team{
					Name:    teamname,
					Members: team.Data.Members,
					Owners:  team.Data.Owners,
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
			teamRepo[*repo.Owner][repo.Metadata.Name] = repo
		}
		for _, r := range repo.Data.Readers {
			if _, ok := teamRepo[r]; !ok {
				teamRepo[r] = make(map[string]*entity.Repository)
			}
			teamRepo[r][repo.Metadata.Name] = repo
		}
		for _, w := range repo.Data.Writers {
			if _, ok := teamRepo[w]; !ok {
				teamRepo[w] = make(map[string]*entity.Repository)
			}
			teamRepo[w][repo.Metadata.Name] = repo
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
			Name:     r.Metadata.Name,
			Public:   r.Data.IsPublic,
			Archived: r.Data.IsArchived,
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
		err, applied := g.serveApply(true)
		if !applied && err == nil {
			// the run was skipped
			g.syncInterval = config.Config.ServerApplyInterval
		} else {
			now := time.Now()
			g.lastSyncTime = &now
			g.lastSyncError = err
			if err != nil {
				logrus.Error(err)
			}
			g.syncInterval = config.Config.ServerApplyInterval
		}
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
				return
			default:
				g.syncInterval--
				time.Sleep(1 * time.Second)
				if g.syncInterval <= 0 {
					// Do some work here
					err, applied := g.serveApply(false)
					if !applied && err == nil {
						// the run was skipped
						g.syncInterval = config.Config.ServerApplyInterval
					} else {
						now := time.Now()
						g.lastSyncTime = &now
						g.lastSyncError = err
						if err != nil {
							logrus.Error(err)
						}
						g.syncInterval = config.Config.ServerApplyInterval
					}
				}
			}
		}
	}()

	// Handle OS signals
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)
	<-signalCh
	fmt.Println("Received OS signal, stopping Goliac...")

	close(stopCh)
	wg.Wait()
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
	err := g.goliac.LoadAndValidateGoliacOrganization(repo, branch)
	defer g.goliac.Close()
	if err != nil {
		return fmt.Errorf("failed to load and validate: %s", err), false
	}

	// we are ready (to give local state, and to sync with remote)
	g.ready = true

	u, err := url.Parse(repo)
	if err != nil {
		return fmt.Errorf("failed to parse %s: %v", repo, err), false
	}
	teamsreponame := strings.TrimSuffix(path.Base(u.Path), filepath.Ext(path.Base(u.Path)))

	err = g.goliac.ApplyToGithub(false, teamsreponame, branch, forceresync)
	if err != nil {
		return fmt.Errorf("failed to apply on branch %s: %s", branch, err), false
	}
	return nil, true
}
