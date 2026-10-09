package security

import (
	"errors"
	"testing"
)

func TestEventDescriptorRegistryIsComplete(t *testing.T) {
	const eventKindCount = 30
	if len(eventDescriptors) != eventKindCount {
		t.Fatalf("descriptor count=%d want=%d", len(eventDescriptors), eventKindCount)
	}
	for kind, descriptor := range eventDescriptors {
		if kind == "" || descriptor.newPayload == nil || descriptor.newPayload() == nil {
			t.Fatalf("descriptor %q has no payload factory", kind)
		}
		if len(descriptor.allowedEffects) == 0 {
			t.Fatalf("descriptor %q has no allowed effects", kind)
		}
		if descriptor.validate == nil {
			t.Fatalf("descriptor %q has no validator", kind)
		}
		if descriptor.apply == nil {
			t.Fatalf("descriptor %q has no applier", kind)
		}
		for _, effect := range descriptor.allowedEffects {
			if _, ok := eventDescriptors[effect]; !ok {
				t.Fatalf("descriptor %q references unsupported effect %q", kind, effect)
			}
		}
	}
}

func TestEventDescriptorRegistryOwnsValidationAndApplicationDispatch(t *testing.T) {
	validationSentinel := errors.New("validation dispatched")
	applicationSentinel := errors.New("application dispatched")
	for kind, descriptor := range eventDescriptors {
		t.Run(string(kind), func(t *testing.T) {
			registry := cloneEventDescriptors(eventDescriptors)
			descriptor.validate = func(Event) error { return validationSentinel }
			descriptor.apply = func(*State, Event) error { return applicationSentinel }
			registry[kind] = descriptor

			if err := dispatchEventValidation(registry, Event{Kind: kind}); !errors.Is(err, validationSentinel) {
				t.Fatalf("validation dispatch error=%v", err)
			}
			if err := dispatchEventApplication(registry, &State{}, Event{Kind: kind}); !errors.Is(err, applicationSentinel) {
				t.Fatalf("application dispatch error=%v", err)
			}
		})
	}
}

func cloneEventDescriptors(source map[EventKind]eventDescriptor) map[EventKind]eventDescriptor {
	result := make(map[EventKind]eventDescriptor, len(source))
	for kind, descriptor := range source {
		result[kind] = descriptor
	}
	return result
}
