package models

import (
	"testing"

	"github.com/keydotcat/backend/util"
)

func getDummyOwnerWithTeam() (*User, *Team) {
	ctx := getCtx()
	owner := getDummyUser()
	ts, err := owner.GetTeams(ctx)
	if err != nil {
		panic(err)
	}
	if len(ts) != 1 {
		panic("Got more than 1 team")
	}
	return owner, ts[0]
}

func TestCreateTeam(t *testing.T) {
	ctx := getCtx()
	owner := getDummyUser()
	vkp := VaultKeyPair{make([]byte, 32), map[string][]byte{owner.Id: []byte("crap")}}
	tName := owner.Id + " other team"
	team1, err := owner.CreateTeam(ctx, tName, vkp)
	if err != nil {
		t.Fatal(err)
	}
	team2, err := owner.CreateTeam(ctx, tName, vkp)
	if err != nil {
		t.Fatal(err)
	}
	if team1.Id == team2.Id {
		t.Errorf("Team IDs match! '%s' == '%s'", team1.Id, team2.Id)
	}
}

func TestInviteUserToTeam(t *testing.T) {
	ctx := getCtx()
	owner, team := getDummyOwnerWithTeam()
	added, err := team.AddOrInviteUserByEmail(ctx, owner, "a@a.com")
	if err != nil {
		t.Fatal(err)
	}
	if added {
		t.Fatalf("Added user when it had to be invited")
	}
	added, err = team.AddOrInviteUserByEmail(ctx, owner, "a@a.com")
	if !util.CheckErr(err, ErrAlreadyInvited) {
		t.Fatalf("Expected error %s and got %s", ErrAlreadyInvited, err)
	}
}

func TestAddExistingUserToTeam(t *testing.T) {
	ctx := getCtx()
	owner, team := getDummyOwnerWithTeam()
	invitee := getDummyUser()
	added, err := team.AddOrInviteUserByEmail(ctx, owner, invitee.Email)
	if err != nil {
		t.Fatal(err)
	}
	if !added {
		t.Fatalf("Added user when it had to be invited")
	}
	added, err = team.AddOrInviteUserByEmail(ctx, owner, invitee.Email)
	if !util.CheckErr(err, ErrAlreadyInTeam) {
		t.Fatalf("Expected error %s and got %s", ErrAlreadyInTeam, err)
	}
}

func TestCreateVault(t *testing.T) {
	ctx := getCtx()
	owner, team := getDummyOwnerWithTeam()
	invitee := getDummyUser()
	_, err := team.AddOrInviteUserByEmail(ctx, owner, invitee.Email)
	if err != nil {
		t.Fatal(err)
	}
	vkp := getDummyVaultKeyPair(owner.Id)
	vname := util.GenerateRandomToken(5)
	vaults, err := team.GetVaultsForUser(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for _, v := range vaults {
		keys = append(keys, v.Id)
	}
	vkp = getDummyVaultKeyPair(keys...)
	err = team.PromoteUser(ctx, owner, invitee, vkp)
	if err != nil {
		t.Fatalf("Unexpected when promoting a user: %s", err)
	}
	_, err = team.CreateVault(ctx, vname, vkp)
	if !util.CheckErr(err, ErrInvalidKeys) {
		t.Fatalf("Unexpected error: %s vs %s", ErrInvalidKeys, err)
	}
	vkp = getDummyVaultKeyPair(owner.Id)
	_, err = team.CreateVault(ctx, vname, vkp)
	if !util.CheckErr(err, ErrInvalidKeys) {
		t.Fatalf("Unexpected error: %s vs %s", ErrInvalidKeys, err)
	}
	vkp = getDummyVaultKeyPair(owner.Id, invitee.Id)
	_, err = team.CreateVault(ctx, vname, vkp)
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}
	vaults, err = team.GetVaultsForUser(ctx, invitee)
	if len(vaults) != 2 {
		t.Fatalf("Invalid number of vaults")
	}
}

func TestPromoteUser(t *testing.T) {
	ctx := getCtx()
	owner, team := getDummyOwnerWithTeam()
	invitee := getDummyUser()
	_, err := team.AddOrInviteUserByEmail(ctx, owner, invitee.Email)
	if err != nil {
		t.Fatal(err)
	}
	vkp := getDummyVaultKeyPair(owner.Id)
	err = team.PromoteUser(ctx, owner, invitee, vkp)
	if !util.CheckErr(err, ErrInvalidKeys) {
		t.Fatalf("Unexpected error: %s vs %s", ErrInvalidKeys, err)
	}
	vaults, err := team.GetVaultsForUser(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for _, v := range vaults {
		keys = append(keys, v.Id)
	}
	vkp = getDummyVaultKeyPair(keys...)
	err = team.PromoteUser(ctx, owner, invitee, vkp)
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}
	isAdmin, err := team.CheckAdmin(ctx, invitee)
	if err != nil {
		t.Fatal(err)
	}
	if !isAdmin {
		t.Fatalf("User was supposed to be an admin!")
	}
	iVaults, err := team.GetVaultsForUser(ctx, invitee)
	if err != nil {
		t.Fatal(err)
	}
	if len(iVaults) != len(vaults) {
		t.Errorf("Expected to have the same vaults for both admins!")
	}
	err = team.DemoteUser(ctx, invitee, owner)
	if !util.CheckErr(err, ErrUnauthorized) {
		t.Fatalf("Unexpected error: %s vs %s", ErrUnauthorized, err)
	}
	err = team.DemoteUser(ctx, owner, invitee)
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}
	isAdmin, err = team.CheckAdmin(ctx, invitee)
	if err != nil {
		t.Fatal(err)
	}
	if isAdmin {
		t.Fatalf("User was supposed NOT to be an admin!")
	}

}