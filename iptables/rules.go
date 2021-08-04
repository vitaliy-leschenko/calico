// Copyright (c) 2016-2019 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package iptables

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"

	log "github.com/sirupsen/logrus"
)

const (
	// Compromise: shorter is better for table occupancy and readability. Longer is better for
	// collision-resistance.  16 chars gives us 96 bits of entropy, which is fairly collision
	// resistant.
	HashLength = 16
)

type Rule struct {
	Match   MatchCriteria
	Action  Action
	Comment []string
}

func (r Rule) RenderAppend(chainName, prefixFragment string, features *Features) string {
	fragments := make([]string, 0, 6)
	fragments = append(fragments, "-A", chainName)
	return r.renderInner(fragments, prefixFragment, features)
}

func (r Rule) RenderInsert(chainName, prefixFragment string, features *Features) string {
	fragments := make([]string, 0, 6)
	fragments = append(fragments, "-I", chainName)
	return r.renderInner(fragments, prefixFragment, features)
}

func (r Rule) RenderInsertAtRuleNumber(chainName string, ruleNum int, prefixFragment string, features *Features) string {
	fragments := make([]string, 0, 7)
	fragments = append(fragments, "-I", chainName, fmt.Sprintf("%d", ruleNum))
	return r.renderInner(fragments, prefixFragment, features)
}

func (r Rule) RenderReplace(chainName string, ruleNum int, prefixFragment string, features *Features) string {
	fragments := make([]string, 0, 7)
	fragments = append(fragments, "-R", chainName, fmt.Sprintf("%d", ruleNum))
	return r.renderInner(fragments, prefixFragment, features)
}

func (r Rule) renderInner(fragments []string, prefixFragment string, features *Features) string {
	if prefixFragment != "" {
		fragments = append(fragments, prefixFragment)
	}
	for _, c := range r.Comment {
		c = escapeComment(c)
		c = truncateComment(c)
		commentFragment := fmt.Sprintf("-m comment --comment \"%s\"", c)
		fragments = append(fragments, commentFragment)
	}
	matchFragment := r.Match.Render()
	if matchFragment != "" {
		fragments = append(fragments, matchFragment)
	}
	actionFragment := r.Action.ToFragment(features)
	if actionFragment != "" {
		fragments = append(fragments, actionFragment)
	}
	return strings.Join(fragments, " ")
}

var shellUnsafe = regexp.MustCompile(`[^\w @%+=:,./-]`)

// escapeComment replaces anything other than "safe" shell characters with an
// underscore (_).  This is a lossy conversion, but the expected use case
// for this stuff getting all the way to iptables are either
//  - hashes/IDs generated by higher layer systems
//  - comments on what the rules do
// which should be fine with this limitation.
// There just isn't a good way to escape this stuff in a way that iptables-restore
// will respect.  strconv.Quote() leaves actual quote characters in the output,
// which break iptables-restore.
func escapeComment(s string) string {
	return shellUnsafe.ReplaceAllString(s, "_")
}

const maxCommentLen = 256

func truncateComment(s string) string {
	if len(s) > maxCommentLen {
		return s[0:maxCommentLen]
	}
	return s
}

type Chain struct {
	Name  string
	Rules []Rule
}

func (c *Chain) RuleHashes(features *Features) []string {
	if c == nil {
		return nil
	}
	hashes := make([]string, len(c.Rules))
	// First hash the chain name so that identical rules in different chains will get different
	// hashes.
	s := sha256.New224()
	_, err := s.Write([]byte(c.Name))
	if err != nil {
		log.WithFields(log.Fields{
			"chain": c.Name,
		}).WithError(err).Panic("Failed to write suffix to hash.")
		return nil
	}

	hash := s.Sum(nil)
	for ii, rule := range c.Rules {
		// Each hash chains in the previous hash, so that its position in the chain and
		// the rules before it affect its hash.
		s.Reset()
		_, err = s.Write(hash)
		if err != nil {
			log.WithFields(log.Fields{
				"action":   rule.Action,
				"position": ii,
				"chain":    c.Name,
			}).WithError(err).Panic("Failed to write suffix to hash.")
		}
		ruleForHashing := rule.RenderAppend(c.Name, "HASH", features)
		_, err = s.Write([]byte(ruleForHashing))
		if err != nil {
			log.WithFields(log.Fields{
				"ruleFragment": ruleForHashing,
				"action":       rule.Action,
				"position":     ii,
				"chain":        c.Name,
			}).WithError(err).Panic("Failed to write rule for hashing.")
		}
		hash = s.Sum(hash[0:0])
		// Encode the hash using a compact character set.  We use the URL-safe base64
		// variant because it uses '-' and '_', which are more shell-friendly.
		hashes[ii] = base64.RawURLEncoding.EncodeToString(hash)[:HashLength]
		if log.GetLevel() >= log.DebugLevel {
			log.WithFields(log.Fields{
				"ruleFragment": ruleForHashing,
				"action":       rule.Action,
				"position":     ii,
				"chain":        c.Name,
				"hash":         hashes[ii],
			}).Debug("Hashed rule")
		}
	}
	return hashes
}

func (c *Chain) IPSetIDs() (ipSetIDs []string) {
	if c == nil {
		return nil
	}
	for _, rule := range c.Rules {
		for _, matchString := range []string(rule.Match) {
			words := strings.Split(matchString, " ")
			for i := range words {
				if words[i] == "--match-set" && (i+1) < len(words) {
					ipSetIDs = append(ipSetIDs, words[i+1])
				}
			}
		}
	}
	return
}
