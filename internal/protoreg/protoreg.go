// Package protoreg references every plugin's protobuf command package so
// that importing it registers their message types in the global registry.
// Programs that translate google.protobuf.Any payloads without linking the
// plugins themselves (the browser gateway) import this package. A test in
// package game checks that every command type the server accepts is listed
// here, so adding a plugin cannot silently break the gateway.
package protoreg

import (
	"google.golang.org/protobuf/reflect/protoreflect"

	attributesv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/attributes/v1"
	cloningv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/cloning/v1"
	energyv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/energy/v1"
	metadatav1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/metadata/v1"
	movementv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/movement/v1"
	upgradesv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/upgrades/v1"
	wellsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/wells/v1"
)

// Files lists the plugin command proto files.
var Files = []protoreflect.FileDescriptor{
	attributesv1.File_rts_attributes_v1_attributes_proto,
	cloningv1.File_rts_cloning_v1_cloning_proto,
	energyv1.File_rts_energy_v1_energy_proto,
	metadatav1.File_rts_metadata_v1_metadata_proto,
	movementv1.File_rts_movement_v1_movement_proto,
	upgradesv1.File_rts_upgrades_v1_upgrades_proto,
	wellsv1.File_rts_wells_v1_wells_proto,
}

// Has reports whether a message with the given full name is declared in
// one of Files.
func Has(fullName string) bool {
	for _, f := range Files {
		msgs := f.Messages()
		for i := range msgs.Len() {
			if string(msgs.Get(i).FullName()) == fullName {
				return true
			}
		}
	}
	return false
}
