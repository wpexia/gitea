// Copyright 2014 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package models

import (
	"container/list"
	"fmt"
	"os/exec"
	"strings"

	"code.gitea.io/git"

	"code.gitea.io/gitea/modules/log"
)

// env keys for git hooks need
const (
	EnvRepoName     = "GITEA_REPO_NAME"
	EnvRepoUsername = "GITEA_REPO_USER_NAME"
	EnvRepoIsWiki   = "GITEA_REPO_IS_WIKI"
	EnvPusherName   = "GITEA_PUSHER_NAME"
	EnvPusherID     = "GITEA_PUSHER_ID"
)

// CommitToPushCommit transforms a git.Commit to PushCommit type.
func CommitToPushCommit(commit *git.Commit) *PushCommit {
	return &PushCommit{
		Sha1:           commit.ID.String(),
		Message:        commit.Message(),
		AuthorEmail:    commit.Author.Email,
		AuthorName:     commit.Author.Name,
		CommitterEmail: commit.Committer.Email,
		CommitterName:  commit.Committer.Name,
		Timestamp:      commit.Author.When,
	}
}

// ListToPushCommits transforms a list.List to PushCommits type.
func ListToPushCommits(l *list.List) *PushCommits {
	var commits []*PushCommit
	var actEmail string
	for e := l.Front(); e != nil; e = e.Next() {
		commit := e.Value.(*git.Commit)
		if actEmail == "" {
			actEmail = commit.Committer.Email
		}
		commits = append(commits, CommitToPushCommit(commit))
	}
	return &PushCommits{l.Len(), commits, "", nil}
}

// PushUpdateOptions defines the push update options
type PushUpdateOptions struct {
	PusherID     int64
	PusherName   string
	RepoUserName string
	RepoName     string
	RefFullName  string
	OldCommitID  string
	NewCommitID  string
}

// PushUpdate must be called for any push actions in order to
// generates necessary push action history feeds.
func PushUpdate(branch string, opt PushUpdateOptions) error {
	repo, err := pushUpdate(opt)
	if err != nil {
		return err
	}

	pusher, err := GetUserByID(opt.PusherID)
	if err != nil {
		return err
	}

	log.Trace("TriggerTask '%s/%s' by %s", repo.Name, branch, pusher.Name)

	go AddTestPullRequestTask(pusher, repo.ID, branch, true)
	return nil
}

func pushUpdateDeleteTag(repo *Repository, gitRepo *git.Repository, tagName string) error {
	rel, err := GetRelease(repo.ID, tagName)
	if err != nil {
		if IsErrReleaseNotExist(err) {
			return nil
		}
		return fmt.Errorf("GetRelease: %v", err)
	}
	if rel.IsTag {
		if _, err = x.Id(rel.ID).Delete(new(Release)); err != nil {
			return fmt.Errorf("Delete: %v", err)
		}
	} else {
		rel.IsDraft = true
		rel.NumCommits = 0
		rel.Sha1 = ""
		if _, err = x.Id(rel.ID).AllCols().Update(rel); err != nil {
			return fmt.Errorf("Update: %v", err)
		}
	}

	return nil
}

func pushUpdateAddTag(repo *Repository, gitRepo *git.Repository, tagName string) error {
	rel, err := GetRelease(repo.ID, tagName)
	if err != nil && !IsErrReleaseNotExist(err) {
		return fmt.Errorf("GetRelease: %v", err)
	}

	tag, err := gitRepo.GetTag(tagName)
	if err != nil {
		return fmt.Errorf("GetTag: %v", err)
	}
	commit, err := tag.Commit()
	if err != nil {
		return fmt.Errorf("Commit: %v", err)
	}
	tagCreatedUnix := commit.Author.When.Unix()

	author, err := GetUserByEmail(commit.Author.Email)
	if err != nil && !IsErrUserNotExist(err) {
		return fmt.Errorf("GetUserByEmail: %v", err)
	}

	commitsCount, err := commit.CommitsCount()
	if err != nil {
		return fmt.Errorf("CommitsCount: %v", err)
	}

	if rel == nil {
		rel = &Release{
			RepoID:       repo.ID,
			Title:        "",
			TagName:      tagName,
			LowerTagName: strings.ToLower(tagName),
			Target:       "",
			Sha1:         commit.ID.String(),
			NumCommits:   commitsCount,
			Note:         "",
			IsDraft:      false,
			IsPrerelease: false,
			IsTag:        true,
			CreatedUnix:  tagCreatedUnix,
		}
		if author != nil {
			rel.PublisherID = author.ID
		}

		if _, err = x.InsertOne(rel); err != nil {
			return fmt.Errorf("InsertOne: %v", err)
		}
	} else {
		rel.Sha1 = commit.ID.String()
		rel.CreatedUnix = tagCreatedUnix
		rel.NumCommits = commitsCount
		rel.IsDraft = false
		if rel.IsTag && author != nil {
			rel.PublisherID = author.ID
		}
		if _, err = x.Id(rel.ID).AllCols().Update(rel); err != nil {
			return fmt.Errorf("Update: %v", err)
		}
	}
	return nil
}

func pushUpdate(opts PushUpdateOptions) (repo *Repository, err error) {
	isNewRef := opts.OldCommitID == git.EmptySHA
	isDelRef := opts.NewCommitID == git.EmptySHA
	if isNewRef && isDelRef {
		return nil, fmt.Errorf("Old and new revisions are both %s", git.EmptySHA)
	}

	repoPath := RepoPath(opts.RepoUserName, opts.RepoName)

	gitUpdate := exec.Command("git", "update-server-info")
	gitUpdate.Dir = repoPath
	if err = gitUpdate.Run(); err != nil {
		return nil, fmt.Errorf("Failed to call 'git update-server-info': %v", err)
	}

	owner, err := GetUserByName(opts.RepoUserName)
	if err != nil {
		return nil, fmt.Errorf("GetUserByName: %v", err)
	}

	repo, err = GetRepositoryByName(owner.ID, opts.RepoName)
	if err != nil {
		return nil, fmt.Errorf("GetRepositoryByName: %v", err)
	}

	gitRepo, err := git.OpenRepository(repoPath)
	if err != nil {
		return nil, fmt.Errorf("OpenRepository: %v", err)
	}

	if err = repo.UpdateSize(); err != nil {
		log.Error(4, "Failed to update size for repository: %v", err)
	}

	var commits = &PushCommits{}
	if strings.HasPrefix(opts.RefFullName, git.TagPrefix) {
		// If is tag reference
		if isDelRef {
			err = pushUpdateDeleteTag(repo, gitRepo, opts.RefFullName[len(git.TagPrefix):])
			if err != nil {
				return nil, fmt.Errorf("pushUpdateDeleteTag: %v", err)
			}
		} else {
			err = pushUpdateAddTag(repo, gitRepo, opts.RefFullName[len(git.TagPrefix):])
			if err != nil {
				return nil, fmt.Errorf("pushUpdateAddTag: %v", err)
			}
		}
	} else if !isDelRef {
		// If is branch reference
		newCommit, err := gitRepo.GetCommit(opts.NewCommitID)
		if err != nil {
			return nil, fmt.Errorf("gitRepo.GetCommit: %v", err)
		}

		// Push new branch.
		var l *list.List
		if isNewRef {
			l, err = newCommit.CommitsBeforeLimit(10)
			if err != nil {
				return nil, fmt.Errorf("newCommit.CommitsBeforeLimit: %v", err)
			}
		} else {
			l, err = newCommit.CommitsBeforeUntil(opts.OldCommitID)
			if err != nil {
				return nil, fmt.Errorf("newCommit.CommitsBeforeUntil: %v", err)
			}
		}

		commits = ListToPushCommits(l)
	}

	if err := CommitRepoAction(CommitRepoActionOptions{
		PusherName:  opts.PusherName,
		RepoOwnerID: owner.ID,
		RepoName:    repo.Name,
		RefFullName: opts.RefFullName,
		OldCommitID: opts.OldCommitID,
		NewCommitID: opts.NewCommitID,
		Commits:     commits,
	}); err != nil {
		return nil, fmt.Errorf("CommitRepoAction: %v", err)
	}
	return repo, nil
}
