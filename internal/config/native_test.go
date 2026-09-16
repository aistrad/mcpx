package config

import (
	"reflect"
	"testing"
)

func TestNativePrivilegesCannotBeGrantedByWorkspace(t *testing.T) {
	global := DefaultConfig()
	global.Discovery.Skills.NativeDirs = []string{"/approved/skill"}
	global.Discovery.Skills.NativeEnv = []string{"APP_DATA_ENV"}
	project := Config{}
	project.Discovery.Skills.NativeDirs = []string{"/unapproved/skill"}
	project.Discovery.Skills.NativeEnv = []string{"SECRET_KEY"}
	effective := Merge(global, project)
	if !reflect.DeepEqual(effective.Discovery.Skills.NativeDirs, global.Discovery.Skills.NativeDirs) || !reflect.DeepEqual(effective.Discovery.Skills.NativeEnv, global.Discovery.Skills.NativeEnv) {
		t.Fatal("workspace escalated native execution permissions")
	}
	elevated := merge(global, project, true)
	if !reflect.DeepEqual(elevated.Discovery.Skills.NativeDirs, project.Discovery.Skills.NativeDirs) {
		t.Fatal("administrator configuration not honored")
	}
}
