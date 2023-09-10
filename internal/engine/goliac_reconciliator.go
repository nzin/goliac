package engine

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/Alayacare/goliac/internal/config"
	"github.com/Alayacare/goliac/internal/entity"
	"github.com/gosimple/slug"
	"github.com/sirupsen/logrus"
)

type key string

const (
	KeyAuthor key = "author"
)

/*
 * GoliacReconciliator is here to sync the local state to the remote state
 */
type GoliacReconciliator interface {
	Reconciliate(ctx context.Context, local GoliacLocal, remote GoliacRemote, teamreponame string, dryrun bool) error
}

type GoliacReconciliatorImpl struct {
	executor   ReconciliatorExecutor
	repoconfig *config.RepositoryConfig
}

func NewGoliacReconciliatorImpl(executor ReconciliatorExecutor, repoconfig *config.RepositoryConfig) GoliacReconciliator {
	return &GoliacReconciliatorImpl{
		executor:   executor,
		repoconfig: repoconfig,
	}
}

func (r *GoliacReconciliatorImpl) Reconciliate(ctx context.Context, local GoliacLocal, remote GoliacRemote, teamsreponame string, dryrun bool) error {
	rremote := NewMutableGoliacRemoteImpl(remote)
	r.Begin(ctx, dryrun)
	err := r.reconciliateUsers(ctx, local, rremote, dryrun)
	if err != nil {
		r.Rollback(ctx, dryrun, err)
		return err
	}

	err = r.reconciliateTeams(ctx, local, rremote, dryrun)
	if err != nil {
		r.Rollback(ctx, dryrun, err)
		return err
	}

	err = r.reconciliateRepositories(ctx, local, rremote, teamsreponame, dryrun)
	if err != nil {
		r.Rollback(ctx, dryrun, err)
		return err
	}

	if remote.IsEnterprise() {
		err = r.reconciliateRulesets(ctx, local, rremote, r.repoconfig, dryrun)
		if err != nil {
			r.Rollback(ctx, dryrun, err)
			return err
		}
	}

	r.Commit(ctx, dryrun)

	return nil
}

/*
 * This function sync teams and team's members
 */
func (r *GoliacReconciliatorImpl) reconciliateUsers(ctx context.Context, local GoliacLocal, remote *MutableGoliacRemoteImpl, dryrun bool) error {
	ghUsers := remote.Users()

	rUsers := make(map[string]string)
	for _, u := range ghUsers {
		rUsers[u] = u
	}

	for _, lUser := range local.Users() {
		user, ok := rUsers[lUser.Spec.GithubID]

		if !ok {
			// deal with non existing remote user
			r.AddUserToOrg(ctx, dryrun, remote, user)
		} else {
			delete(rUsers, user)
		}
	}

	// remaining (GH) users (aka not found locally)
	for _, rUser := range rUsers {
		// DELETE User
		r.RemoveUserFromOrg(ctx, dryrun, remote, rUser)
	}
	return nil
}

/*
 * This function sync teams and team's members
 */
func (r *GoliacReconciliatorImpl) reconciliateTeams(ctx context.Context, local GoliacLocal, remote *MutableGoliacRemoteImpl, dryrun bool) error {
	ghTeams := remote.Teams()

	rTeams := make(map[string]*GithubTeam)
	for k, v := range ghTeams {
		rTeams[k] = v
	}

	// prepare the teams we want (regular and "-owners")
	slugTeams := make(map[string]*GithubTeam)
	for teamname, teamvalue := range local.Teams() {
		members := []string{}
		members = append(members, teamvalue.Spec.Members...)
		members = append(members, teamvalue.Spec.Owners...)

		teamslug := slug.Make(teamname)
		slugTeams[teamslug] = &GithubTeam{
			Name:    teamname,
			Slug:    teamslug,
			Members: members,
		}

		// owners
		slugTeams[teamslug+"-owners"] = &GithubTeam{
			Name:    teamname + "-owners",
			Slug:    teamslug + "-owners",
			Members: teamvalue.Spec.Owners,
		}
	}

	// adding the "everyone" team
	if r.repoconfig.EveryoneTeamEnabled {
		everyone := GithubTeam{
			Name:    "everyone",
			Slug:    "everyone",
			Members: []string{},
		}
		for u := range local.Users() {
			everyone.Members = append(everyone.Members, u)
		}
		slugTeams["everyone"] = &everyone
	}

	// now we compare local (slugTeams) and remote (rTeams)

	compareTeam := func(lTeam *GithubTeam, rTeam *GithubTeam) bool {
		res, _, _ := entity.StringArrayEquivalent(lTeam.Members, rTeam.Members)
		return res
	}

	onAdded := func(key string, lTeam *GithubTeam, rTeam *GithubTeam) {
		members := make([]string, 0)
		for _, m := range lTeam.Members {
			if ghuserid, ok := local.Users()[m]; ok {
				members = append(members, ghuserid.Spec.GithubID)
			}
		}
		// CREATE team
		r.CreateTeam(ctx, dryrun, remote, lTeam.Slug, lTeam.Name, members)
	}

	onRemoved := func(key string, lTeam *GithubTeam, rTeam *GithubTeam) {
		// DELETE team
		r.DeleteTeam(ctx, dryrun, remote, rTeam.Slug)
	}

	onChanged := func(slugTeam string, lTeam *GithubTeam, rTeam *GithubTeam) {
		localMembers := make(map[string]bool)
		for _, m := range lTeam.Members {
			if ghuserid, ok := local.Users()[m]; ok {
				localMembers[ghuserid.Spec.GithubID] = true
			}
		}

		for _, m := range rTeam.Members {
			if _, ok := localMembers[m]; !ok {
				// REMOVE team member
				r.UpdateTeamRemoveMember(ctx, dryrun, remote, slugTeam, m)
			} else {
				delete(localMembers, m)
			}
		}
		for m := range localMembers {
			// ADD team member
			r.UpdateTeamAddMember(ctx, dryrun, remote, slugTeam, m, "member")
		}
	}

	CompareEntities(slugTeams, rTeams, compareTeam, onAdded, onRemoved, onChanged)

	return nil
}

type GithubRepoComparable struct {
	IsPublic            bool
	IsArchived          bool
	Writers             []string
	Readers             []string
	ExternalUserReaders []string // githubids
	ExternalUserWriters []string // githubids
}

/*
 * This function sync repositories and team's repositories permissions
 */
func (r *GoliacReconciliatorImpl) reconciliateRepositories(ctx context.Context, local GoliacLocal, remote *MutableGoliacRemoteImpl, teamsreponame string, dryrun bool) error {
	ghRepos := remote.Repositories()
	rRepos := make(map[string]*GithubRepoComparable)
	for k, v := range ghRepos {
		repo := &GithubRepoComparable{
			IsPublic:            !v.IsPrivate,
			IsArchived:          v.IsArchived,
			Writers:             []string{},
			Readers:             []string{},
			ExternalUserReaders: []string{},
			ExternalUserWriters: []string{},
		}

		for cGithubid, cPermission := range v.ExternalUsers {
			if cPermission == "WRITE" {
				repo.ExternalUserWriters = append(repo.ExternalUserWriters, cGithubid)
			} else {
				repo.ExternalUserReaders = append(repo.ExternalUserReaders, cGithubid)
			}
		}

		rRepos[k] = repo
	}

	// on the remote object, I have teams->repos, and I need repos->teams
	for t, repos := range remote.TeamRepositories() {
		for r, p := range repos {
			if rr, ok := rRepos[r]; ok {
				if p.Permission == "ADMIN" || p.Permission == "WRITE" {
					rr.Writers = append(rr.Writers, t)
				} else {
					rr.Readers = append(rr.Readers, t)
				}
			}
		}
	}

	lRepos := make(map[string]*GithubRepoComparable)
	for reponame, lRepo := range local.Repositories() {
		writers := make([]string, 0)
		for _, w := range lRepo.Spec.Writers {
			writers = append(writers, slug.Make(w))
		}
		// add the team owner's name ;-)
		if lRepo.Owner != nil {
			writers = append(writers, slug.Make(*lRepo.Owner))
		}
		readers := make([]string, 0)
		for _, r := range lRepo.Spec.Readers {
			readers = append(readers, slug.Make(r))
		}

		// special case for the Goliac "teams" repo
		if reponame == teamsreponame {
			for teamname := range local.Teams() {
				writers = append(writers, slug.Make(teamname)+"-owners")
			}
		}

		// adding the "everyone" team to each repository
		if r.repoconfig.EveryoneTeamEnabled {
			readers = append(readers, "everyone")
		}

		// adding exernal reader/writer
		eReaders := make([]string, 0)
		for _, r := range lRepo.Spec.ExternalUserReaders {
			if user, ok := local.ExternalUsers()[r]; ok {
				eReaders = append(eReaders, user.Spec.GithubID)
			}
		}

		eWriters := make([]string, 0)
		for _, w := range lRepo.Spec.ExternalUserWriters {
			if user, ok := local.ExternalUsers()[w]; ok {
				eWriters = append(eWriters, user.Spec.GithubID)
			}
		}

		lRepos[slug.Make(reponame)] = &GithubRepoComparable{
			IsPublic:            lRepo.Spec.IsPublic,
			IsArchived:          lRepo.Archived,
			Readers:             readers,
			Writers:             writers,
			ExternalUserReaders: eReaders,
			ExternalUserWriters: eWriters,
		}
	}

	// now we compare local (slugTeams) and remote (rTeams)

	compareRepos := func(lRepo *GithubRepoComparable, rRepo *GithubRepoComparable) bool {
		if lRepo.IsArchived != rRepo.IsArchived {
			return false
		}
		if lRepo.IsPublic != rRepo.IsPublic {
			return false
		}

		if res, _, _ := entity.StringArrayEquivalent(lRepo.Readers, rRepo.Readers); !res {
			return false
		}

		if res, _, _ := entity.StringArrayEquivalent(lRepo.Writers, rRepo.Writers); !res {
			return false
		}

		if res, _, _ := entity.StringArrayEquivalent(lRepo.ExternalUserReaders, rRepo.ExternalUserReaders); !res {
			return false
		}

		if res, _, _ := entity.StringArrayEquivalent(lRepo.ExternalUserWriters, rRepo.ExternalUserWriters); !res {
			return false
		}

		return true
	}

	onAdded := func(reponame string, lRepo *GithubRepoComparable, rRepo *GithubRepoComparable) {
		// CREATE repository
		r.CreateRepository(ctx, dryrun, remote, reponame, reponame, lRepo.Writers, lRepo.Readers, lRepo.IsPublic)
	}

	onRemoved := func(reponame string, lRepo *GithubRepoComparable, rRepo *GithubRepoComparable) {
		r.DeleteRepository(ctx, dryrun, remote, reponame)
	}

	onChanged := func(reponame string, lRepo *GithubRepoComparable, rRepo *GithubRepoComparable) {
		// reconciliate repositories public/private
		if lRepo.IsPublic != rRepo.IsPublic {
			// UPDATE private repository
			r.UpdateRepositoryUpdatePrivate(ctx, dryrun, remote, reponame, !lRepo.IsPublic)
		}

		// reconciliate repositories archived
		if lRepo.IsArchived != rRepo.IsArchived {
			// UPDATE archived repository
			r.UpdateRepositoryUpdateArchived(ctx, dryrun, remote, reponame, lRepo.IsArchived)
		}

		if res, readToRemove, readToAdd := entity.StringArrayEquivalent(lRepo.Readers, rRepo.Readers); !res {
			for _, teamSlug := range readToAdd {
				r.UpdateRepositoryAddTeamAccess(ctx, dryrun, remote, reponame, teamSlug, "pull")
			}
			for _, teamSlug := range readToRemove {
				r.UpdateRepositoryRemoveTeamAccess(ctx, dryrun, remote, reponame, teamSlug)
			}
		}

		if res, writeToRemove, writeToAdd := entity.StringArrayEquivalent(lRepo.Writers, rRepo.Writers); !res {
			for _, teamSlug := range writeToAdd {
				r.UpdateRepositoryAddTeamAccess(ctx, dryrun, remote, reponame, teamSlug, "push")
			}
			for _, teamSlug := range writeToRemove {
				r.UpdateRepositoryRemoveTeamAccess(ctx, dryrun, remote, reponame, teamSlug)
			}
		}

		resEreader, ereaderToRemove, ereaderToAdd := entity.StringArrayEquivalent(lRepo.ExternalUserReaders, rRepo.ExternalUserReaders)
		resEWriter, ewriteToRemove, ewriteToAdd := entity.StringArrayEquivalent(lRepo.ExternalUserWriters, rRepo.ExternalUserWriters)

		if !resEreader {
			for _, eReader := range ereaderToRemove {
				// check if it is added in the writers
				found := false
				for _, eWriter := range ewriteToAdd {
					if eWriter == eReader {
						found = true
						break
					}
				}
				if !found {
					r.UpdateRepositoryRemoveExternalUser(ctx, dryrun, remote, reponame, eReader)
				}
			}
			for _, eReader := range ereaderToAdd {
				r.UpdateRepositorySetExternalUser(ctx, dryrun, remote, reponame, eReader, "pull")
			}
		}

		if !resEWriter {
			for _, eWriter := range ewriteToRemove {
				// check if it is added in the writers
				found := false
				for _, eReader := range ereaderToAdd {
					if eReader == eWriter {
						found = true
						break
					}
				}
				if !found {
					r.UpdateRepositoryRemoveExternalUser(ctx, dryrun, remote, reponame, eWriter)
				}
			}
			for _, eWriter := range ewriteToAdd {
				r.UpdateRepositorySetExternalUser(ctx, dryrun, remote, reponame, eWriter, "push")
			}
		}

	}

	CompareEntities(lRepos, rRepos, compareRepos, onAdded, onRemoved, onChanged)

	return nil
}

func (r *GoliacReconciliatorImpl) reconciliateRulesets(ctx context.Context, local GoliacLocal, remote *MutableGoliacRemoteImpl, conf *config.RepositoryConfig, dryrun bool) error {
	repositories := local.Repositories()

	lgrs := map[string]*GithubRuleSet{}
	// prepare local comparable
	for _, confrs := range conf.Rulesets {
		match, err := regexp.Compile(confrs.Pattern)
		if err != nil {
			return fmt.Errorf("Not able to parse ruleset regular expression %s: %v", confrs.Pattern, err)
		}
		rs, ok := local.RuleSets()[confrs.Ruleset]
		if !ok {
			return fmt.Errorf("Not able to find ruleset %s definition", confrs.Ruleset)
		}

		grs := GithubRuleSet{
			Name:        rs.Name,
			Enforcement: rs.Spec.Enforcement,
			BypassApps:  map[string]string{},
			OnInclude:   rs.Spec.On.Include,
			OnExclude:   rs.Spec.On.Exclude,
			Rules:       map[string]entity.RuleSetParameters{},
		}
		for _, b := range rs.Spec.BypassApps {
			grs.BypassApps[b.AppName] = b.Mode
		}
		for _, r := range rs.Spec.Rules {
			grs.Rules[r.Ruletype] = r.Parameters
		}
		for reponame := range repositories {
			if match.Match([]byte(slug.Make(reponame))) {
				grs.Repositories = append(grs.Repositories, slug.Make(reponame))
			}
		}
		lgrs[rs.Name] = &grs
	}

	// prepare remote comparable
	rgrs := remote.RuleSets()

	// prepare the diff computation

	compareRulesets := func(lrs *GithubRuleSet, rrs *GithubRuleSet) bool {
		if lrs.Enforcement != rrs.Enforcement {
			return false
		}
		if len(lrs.BypassApps) != len(rrs.BypassApps) {
			return false
		}
		for k, v := range lrs.BypassApps {
			if rrs.BypassApps[k] != v {
				return false
			}
		}
		if res, _, _ := entity.StringArrayEquivalent(lrs.OnInclude, rrs.OnInclude); !res {
			return false
		}
		if res, _, _ := entity.StringArrayEquivalent(lrs.OnExclude, rrs.OnExclude); !res {
			return false
		}
		if len(lrs.Rules) != len(rrs.Rules) {
			return false
		}
		for k, v := range lrs.Rules {
			if !entity.CompareRulesetParameters(k, v, rrs.Rules[k]) {
				return false
			}
		}
		if res, _, _ := entity.StringArrayEquivalent(lrs.Repositories, rrs.Repositories); !res {
			return false
		}

		return true
	}

	onAdded := func(rulesetname string, lRuleset *GithubRuleSet, rRuleset *GithubRuleSet) {
		// CREATE ruleset

		r.AddRuleset(ctx, dryrun, lRuleset)
	}

	onRemoved := func(rulesetname string, lRuleset *GithubRuleSet, rRuleset *GithubRuleSet) {
		// DELETE ruleset
		r.DeleteRuleset(ctx, dryrun, rRuleset.Id)
	}

	onChanged := func(rulesetname string, lRuleset *GithubRuleSet, rRuleset *GithubRuleSet) {
		// UPDATE ruleset
		lRuleset.Id = rRuleset.Id
		r.UpdateRuleset(ctx, dryrun, lRuleset)
	}

	CompareEntities(lgrs, rgrs, compareRulesets, onAdded, onRemoved, onChanged)

	return nil
}

func (r *GoliacReconciliatorImpl) AddUserToOrg(ctx context.Context, dryrun bool, remote *MutableGoliacRemoteImpl, ghuserid string) {
	author := "unknown"
	if a := ctx.Value(KeyAuthor); a != nil {
		author = a.(string)
	}
	logrus.WithFields(map[string]interface{}{"dryrun": dryrun, "author": author, "command": "add_user_to_org"}).Infof("ghusername: %s", ghuserid)
	remote.AddUserToOrg(ghuserid)
	if r.executor != nil {
		r.executor.AddUserToOrg(dryrun, ghuserid)
	}
}

func (r *GoliacReconciliatorImpl) RemoveUserFromOrg(ctx context.Context, dryrun bool, remote *MutableGoliacRemoteImpl, ghuserid string) {
	author := "unknown"
	if a := ctx.Value(KeyAuthor); a != nil {
		author = a.(string)
	}
	logrus.WithFields(map[string]interface{}{"dryrun": dryrun, "author": author, "command": "remove_user_from_org"}).Infof("ghusername: %s", ghuserid)
	remote.RemoveUserFromOrg(ghuserid)
	if r.executor != nil {
		if r.repoconfig.DestructiveOperations.AllowDestructiveUsers {
			r.executor.RemoveUserFromOrg(dryrun, ghuserid)
		}
	}
}

func (r *GoliacReconciliatorImpl) CreateTeam(ctx context.Context, dryrun bool, remote *MutableGoliacRemoteImpl, teamname string, description string, members []string) {
	author := "unknown"
	if a := ctx.Value(KeyAuthor); a != nil {
		author = a.(string)
	}
	logrus.WithFields(map[string]interface{}{"dryrun": dryrun, "author": author, "command": "create_team"}).Infof("teamname: %s, members: %s", teamname, strings.Join(members, ","))
	remote.CreateTeam(teamname, description, members)
	if r.executor != nil {
		r.executor.CreateTeam(dryrun, teamname, description, members)
	}
}
func (r *GoliacReconciliatorImpl) UpdateTeamAddMember(ctx context.Context, dryrun bool, remote *MutableGoliacRemoteImpl, teamslug string, username string, role string) {
	author := "unknown"
	if a := ctx.Value(KeyAuthor); a != nil {
		author = a.(string)
	}
	logrus.WithFields(map[string]interface{}{"dryrun": dryrun, "author": author, "command": "update_team_add_member"}).Infof("teamslug: %s, username: %s, role: %s", teamslug, username, role)
	remote.UpdateTeamAddMember(teamslug, username, "member")
	if r.executor != nil {
		r.executor.UpdateTeamAddMember(dryrun, teamslug, username, "member")
	}
}
func (r *GoliacReconciliatorImpl) UpdateTeamRemoveMember(ctx context.Context, dryrun bool, remote *MutableGoliacRemoteImpl, teamslug string, username string) {
	author := "unknown"
	if a := ctx.Value(KeyAuthor); a != nil {
		author = a.(string)
	}
	logrus.WithFields(map[string]interface{}{"dryrun": dryrun, "author": author, "command": "update_team_remove_member"}).Infof("teamslug: %s, username: %s", teamslug, username)
	remote.UpdateTeamRemoveMember(teamslug, username)
	if r.executor != nil {
		r.executor.UpdateTeamRemoveMember(dryrun, teamslug, username)
	}
}
func (r *GoliacReconciliatorImpl) DeleteTeam(ctx context.Context, dryrun bool, remote *MutableGoliacRemoteImpl, teamslug string) {
	author := "unknown"
	if a := ctx.Value(KeyAuthor); a != nil {
		author = a.(string)
	}
	if r.repoconfig.DestructiveOperations.AllowDestructiveTeams {
		logrus.WithFields(map[string]interface{}{"dryrun": dryrun, "author": author, "command": "delete_team"}).Infof("teamslug: %s", teamslug)
		remote.DeleteTeam(teamslug)
		if r.executor != nil {
			r.executor.DeleteTeam(dryrun, teamslug)
		}
	}
}
func (r *GoliacReconciliatorImpl) CreateRepository(ctx context.Context, dryrun bool, remote *MutableGoliacRemoteImpl, reponame string, descrition string, writers []string, readers []string, public bool) {
	author := "unknown"
	if a := ctx.Value(KeyAuthor); a != nil {
		author = a.(string)
	}
	logrus.WithFields(map[string]interface{}{"dryrun": dryrun, "author": author, "command": "create_repository"}).Infof("repositoryname: %s, readers: %s, writers: %s, public: %v", reponame, strings.Join(readers, ","), strings.Join(writers, ","), public)
	remote.CreateRepository(reponame, reponame, writers, readers, public)
	if r.executor != nil {
		r.executor.CreateRepository(dryrun, reponame, reponame, writers, readers, public)
	}
}
func (r *GoliacReconciliatorImpl) UpdateRepositoryAddTeamAccess(ctx context.Context, dryrun bool, remote *MutableGoliacRemoteImpl, reponame string, teamslug string, permission string) {
	author := "unknown"
	if a := ctx.Value(KeyAuthor); a != nil {
		author = a.(string)
	}
	logrus.WithFields(map[string]interface{}{"dryrun": dryrun, "author": author, "command": "update_repository_add_team"}).Infof("repositoryname: %s, teamslug: %s, permission: %s", reponame, teamslug, permission)
	remote.UpdateRepositoryAddTeamAccess(reponame, teamslug, permission)
	if r.executor != nil {
		r.executor.UpdateRepositoryAddTeamAccess(dryrun, reponame, teamslug, permission)
	}
}

func (r *GoliacReconciliatorImpl) UpdateRepositoryUpdateTeamAccess(ctx context.Context, dryrun bool, remote *MutableGoliacRemoteImpl, reponame string, teamslug string, permission string) {
	author := "unknown"
	if a := ctx.Value(KeyAuthor); a != nil {
		author = a.(string)
	}
	logrus.WithFields(map[string]interface{}{"dryrun": dryrun, "author": author, "command": "update_repository_update_team"}).Infof("repositoryname: %s, teamslug:%s, permission: %s", reponame, teamslug, permission)
	remote.UpdateRepositoryUpdateTeamAccess(reponame, teamslug, permission)
	if r.executor != nil {
		r.executor.UpdateRepositoryUpdateTeamAccess(dryrun, reponame, teamslug, permission)
	}
}
func (r *GoliacReconciliatorImpl) UpdateRepositoryRemoveTeamAccess(ctx context.Context, dryrun bool, remote *MutableGoliacRemoteImpl, reponame string, teamslug string) {
	author := "unknown"
	if a := ctx.Value(KeyAuthor); a != nil {
		author = a.(string)
	}
	logrus.WithFields(map[string]interface{}{"dryrun": dryrun, "author": author, "command": "update_repository_remove_team"}).Infof("repositoryname: %s, teamslug:%s", reponame, teamslug)
	remote.UpdateRepositoryRemoveTeamAccess(reponame, teamslug)
	if r.executor != nil {
		r.executor.UpdateRepositoryRemoveTeamAccess(dryrun, reponame, teamslug)
	}
}

func (r *GoliacReconciliatorImpl) DeleteRepository(ctx context.Context, dryrun bool, remote *MutableGoliacRemoteImpl, reponame string) {
	author := "unknown"
	if a := ctx.Value(KeyAuthor); a != nil {
		author = a.(string)
	}
	if r.repoconfig.DestructiveOperations.AllowDestructiveRepositories {
		logrus.WithFields(map[string]interface{}{"dryrun": dryrun, "author": author, "command": "delete_repository"}).Infof("repositoryname: %s", reponame)
		remote.DeleteRepository(reponame)
		if r.executor != nil {
			r.executor.DeleteRepository(dryrun, reponame)
		}
	}
}
func (r *GoliacReconciliatorImpl) UpdateRepositoryUpdatePrivate(ctx context.Context, dryrun bool, remote *MutableGoliacRemoteImpl, reponame string, private bool) {
	author := "unknown"
	if a := ctx.Value(KeyAuthor); a != nil {
		author = a.(string)
	}
	logrus.WithFields(map[string]interface{}{"dryrun": dryrun, "author": author, "command": "update_repository_update_private"}).Infof("repositoryname: %s private:%v", reponame, private)
	remote.UpdateRepositoryUpdatePrivate(reponame, private)
	if r.executor != nil {
		r.executor.UpdateRepositoryUpdatePrivate(dryrun, reponame, private)
	}
}
func (r *GoliacReconciliatorImpl) UpdateRepositoryUpdateArchived(ctx context.Context, dryrun bool, remote *MutableGoliacRemoteImpl, reponame string, archived bool) {
	author := "unknown"
	if a := ctx.Value(KeyAuthor); a != nil {
		author = a.(string)
	}
	logrus.WithFields(map[string]interface{}{"dryrun": dryrun, "author": author, "command": "update_repository_update_archived"}).Infof("repositoryname: %s archived:%v", reponame, archived)
	remote.UpdateRepositoryUpdateArchived(reponame, archived)
	if r.executor != nil {
		r.executor.UpdateRepositoryUpdateArchived(dryrun, reponame, archived)
	}
}
func (r *GoliacReconciliatorImpl) AddRuleset(ctx context.Context, dryrun bool, ruleset *GithubRuleSet) {
	author := "unknown"
	if a := ctx.Value(KeyAuthor); a != nil {
		author = a.(string)
	}
	logrus.WithFields(map[string]interface{}{"dryrun": dryrun, "author": author, "command": "add_ruleset"}).Infof("ruleset: %s (id: %d) enforcement: %s", ruleset.Name, ruleset.Id, ruleset.Enforcement)
	if r.executor != nil {
		r.executor.AddRuleset(dryrun, ruleset)
	}
}
func (r *GoliacReconciliatorImpl) UpdateRuleset(ctx context.Context, dryrun bool, ruleset *GithubRuleSet) {
	author := "unknown"
	if a := ctx.Value(KeyAuthor); a != nil {
		author = a.(string)
	}
	logrus.WithFields(map[string]interface{}{"dryrun": dryrun, "author": author, "command": "update_ruleset"}).Infof("ruleset: %s (id: %d) enforcement: %s", ruleset.Name, ruleset.Id, ruleset.Enforcement)
	if r.executor != nil {
		r.executor.UpdateRuleset(dryrun, ruleset)
	}
}
func (r *GoliacReconciliatorImpl) DeleteRuleset(ctx context.Context, dryrun bool, rulesetid int) {
	author := "unknown"
	if a := ctx.Value(KeyAuthor); a != nil {
		author = a.(string)
	}
	if r.repoconfig.DestructiveOperations.AllowDestructiveRulesets {
		logrus.WithFields(map[string]interface{}{"dryrun": dryrun, "author": author, "command": "delete_ruleset"}).Infof("ruleset id:%d", rulesetid)
		if r.executor != nil {
			r.executor.DeleteRuleset(dryrun, rulesetid)
		}
	}
}
func (r *GoliacReconciliatorImpl) UpdateRepositorySetExternalUser(ctx context.Context, dryrun bool, remote *MutableGoliacRemoteImpl, reponame string, collaboatorGithubId string, permission string) {
	author := "unknown"
	if a := ctx.Value(KeyAuthor); a != nil {
		author = a.(string)
	}
	logrus.WithFields(map[string]interface{}{"dryrun": dryrun, "author": author, "command": "update_repository_set_external_user"}).Infof("repositoryname: %s collaborator:%s permission:%s", reponame, collaboatorGithubId, permission)
	remote.UpdateRepositorySetExternalUser(reponame, collaboatorGithubId, permission)
	if r.executor != nil {
		r.executor.UpdateRepositorySetExternalUser(dryrun, reponame, collaboatorGithubId, permission)
	}
}
func (r *GoliacReconciliatorImpl) UpdateRepositoryRemoveExternalUser(ctx context.Context, dryrun bool, remote *MutableGoliacRemoteImpl, reponame string, collaboatorGithubId string) {
	author := "unknown"
	if a := ctx.Value(KeyAuthor); a != nil {
		author = a.(string)
	}
	logrus.WithFields(map[string]interface{}{"dryrun": dryrun, "author": author, "command": "update_repository_remove_external_user"}).Infof("repositoryname: %s collaborator:%s", reponame, collaboatorGithubId)
	remote.UpdateRepositoryRemoveExternalUser(reponame, collaboatorGithubId)
	if r.executor != nil {
		r.executor.UpdateRepositoryRemoveExternalUser(dryrun, reponame, collaboatorGithubId)
	}
}
func (r *GoliacReconciliatorImpl) Begin(ctx context.Context, dryrun bool) {
	logrus.WithFields(map[string]interface{}{"dryrun": dryrun}).Debugf("reconciliation begin")
	if r.executor != nil {
		r.executor.Begin(dryrun)
	}
}
func (r *GoliacReconciliatorImpl) Rollback(ctx context.Context, dryrun bool, err error) {
	logrus.WithFields(map[string]interface{}{"dryrun": dryrun}).Debugf("reconciliation rollback")
	if r.executor != nil {
		r.executor.Rollback(dryrun, err)
	}
}
func (r *GoliacReconciliatorImpl) Commit(ctx context.Context, dryrun bool) {
	logrus.WithFields(map[string]interface{}{"dryrun": dryrun}).Debugf("reconciliation commit")
	if r.executor != nil {
		r.executor.Commit(dryrun)
	}
}
