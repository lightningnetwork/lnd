package actor

import (
	"context"
	"errors"
	"sync"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// stoppable defines an interface for components that can be stopped.
// This is unexported as it's an internal detail of ActorSystem for managing
// actors that need to be shut down.
type stoppable interface {
	Stop()
}

// SystemConfig holds configuration parameters for the ActorSystem.
type SystemConfig struct {
	// MailboxCapacity is the default capacity for actor mailboxes.
	MailboxCapacity int
}

// DefaultConfig returns a default configuration for the ActorSystem.
func DefaultConfig() SystemConfig {
	return SystemConfig{
		MailboxCapacity: 100,
	}
}

// ActorSystem manages the lifecycle of actors and provides coordination
// services such as a receptionist for actor discovery and a dead letter office
// for undeliverable messages. It also handles the graceful shutdown of all
// managed actors.
type ActorSystem struct {
	// receptionist is used for actor discovery.
	receptionist *Receptionist

	// actors stores all actors managed by the system, keyed by their ID.
	// This includes the deadLetterActor.
	actors map[string]stoppable

	// deadLetterActor handles undeliverable messages.
	deadLetterActor ActorRef[Message, any]

	// config holds the system-wide configuration.
	config SystemConfig

	// mu protects the 'actors' map.
	mu sync.RWMutex

	// ctx is the main context for the actor system.
	ctx context.Context

	// cancel cancels the main system context.
	cancel context.CancelFunc
}

// NewActorSystem creates a new actor system using the default configuration.
func NewActorSystem() *ActorSystem {
	return NewActorSystemWithConfig(DefaultConfig())
}

// NewActorSystemWithConfig creates a new actor system with custom configuration
func NewActorSystemWithConfig(config SystemConfig) *ActorSystem {
	ctx, cancel := context.WithCancel(context.Background())

	// Initialize the core ActorSystem components.
	system := &ActorSystem{
		receptionist: newReceptionist(),
		config:       config,
		actors:       make(map[string]stoppable),
		ctx:          ctx,
		cancel:       cancel,
	}

	// Define the behavior for the dead letter actor. It simply returns an
	// error indicating the message was undeliverable.
	deadLetterBehavior := NewFunctionBehavior(
		func(ctx context.Context, msg Message) fn.Result[any] {
			return fn.Err[any](errors.New(
				"message undeliverable: " + msg.MessageType(),
			))
		},
	)

	// Create the raw dead letter actor (*Actor instance). The DLO's own DLO
	// reference is nil to prevent loops if messages to the DLO itself fail.
	deadLetterActorCfg := ActorConfig[Message, any]{
		ID:          "dead-letters",
		Behavior:    deadLetterBehavior,
		DLO:         nil,
		MailboxSize: config.MailboxCapacity,
	}
	deadLetterRawActor := NewActor[Message, any](deadLetterActorCfg)
	deadLetterRawActor.Start()
	system.deadLetterActor = deadLetterRawActor.Ref()

	// Add the raw actor to the map of stoppable actors. No lock needed here
	// as 'system' is not yet accessible concurrently.
	system.actors[deadLetterRawActor.id] = deadLetterRawActor

	// The system is now fully initialized and ready.
	return system
}

// RegisterWithSystem creates an actor with the given ID, service key, and
// behavior within the specified ActorSystem. It starts the actor, adds it to
// the system's management, registers it with the receptionist using the
// provided key, and returns its ActorRef.
func RegisterWithSystem[M Message, R any](as *ActorSystem, id string, key ServiceKey[M, R],
	behavior ActorBehavior[M, R],
) ActorRef[M, R] {

	actorCfg := ActorConfig[M, R]{
		ID:          id,
		Behavior:    behavior,
		DLO:         as.deadLetterActor,
		MailboxSize: as.config.MailboxCapacity,
	}
	actorInstance := NewActor(actorCfg)
	actorInstance.Start()

	// Add the actor instance to the system's list of stoppable actors.
	// This map is protected by the system's mutex.
	as.mu.Lock()
	as.actors[actorInstance.id] = actorInstance
	as.mu.Unlock()

	// Register the actor's reference with the receptionist under the given
	// service key, making it discoverable by other parts of the system.
	RegisterWithReceptionist(as.receptionist, key, actorInstance.Ref())

	return actorInstance.Ref()
}

// Receptionist returns the system's receptionist, which can be used for
// actor service discovery (finding actors by ServiceKey).
func (as *ActorSystem) Receptionist() *Receptionist {
	return as.receptionist
}

// DeadLetters returns a reference to the system's dead letter actor. Messages
// that cannot be delivered to their intended recipient (e.g., if an Ask
// context is cancelled before enqueuing) may be routed here if not otherwise
// handled.
func (as *ActorSystem) DeadLetters() ActorRef[Message, any] {
	return as.deadLetterActor
}

// Shutdown gracefully stops the actor system. It iterates through all managed
// actors, including the dead letter actor, and calls their Stop method.
// After initiating the stop for all actors, it cancels the main system context.
// This method is safe for concurrent use.
func (as *ActorSystem) Shutdown() error {
	// Create a slice of actors to stop. This avoids holding the lock while
	// calling Stop() on each actor, and includes the dead letter actor.
	var actorsToStop []stoppable
	as.mu.RLock()
	for _, actor := range as.actors {
		actorsToStop = append(actorsToStop, actor)
	}
	as.mu.RUnlock()

	// Notify all managed actors to stop. Actor.Stop() is non-blocking.
	// Each actor's Stop method will cancel its internal context, leading
	// to the termination of its processing goroutine.
	for _, actor := range actorsToStop {
		actor.Stop()
	}

	// Clear the actors map after initiating their shutdown.
	as.mu.Lock()
	as.actors = nil
	as.mu.Unlock()

	// Finally cancel the main context
	// This signals to any other components observing the system's context
	// that shutdown has been initiated.
	as.cancel()

	return nil
}

// StopAndRemoveActor stops a specific actor by its ID and removes it from the
// ActorSystem's management. It returns true if the actor was found and stopped,
// false otherwise.
func (as *ActorSystem) StopAndRemoveActor(id string) bool {
	as.mu.Lock()
	defer as.mu.Unlock()

	actorToStop, exists := as.actors[id]
	if !exists {
		return false
	}

	// Stop the actor. This is non-blocking.
	actorToStop.Stop()

	// Remove from the system's management.
	delete(as.actors, id)

	return true
}

// UnregisterFromReceptionist removes an actor reference from a service key in
// the given receptionist. It returns true if the reference was found and
// removed, and false otherwise. This is a package-level generic function
// because methods cannot have their own type parameters in Go.
func UnregisterFromReceptionist[M Message, R any](r *Receptionist,
	key ServiceKey[M, R], refToRemove ActorRef[M, R]) bool {

	r.mu.Lock()
	defer r.mu.Unlock()

	refs, exists := r.registrations[key.name]
	if !exists {
		return false
	}

	found := false

	// Build a new slice containing only the references that are not the one
	// to be removed.
	newRefs := make([]any, 0, len(refs)-1) // Pre-allocate assuming one removal
	for _, itemInSlice := range refs {     // itemInSlice is of type 'any'
		// Try to assert the item from the slice to the specific
		// ActorRef[M,R] type we are trying to remove.
		if specificActorRef, ok := itemInSlice.(ActorRef[M, R]); ok {
			// If the type assertion is successful and it's the one
			// we want to remove, mark as found and skip adding it
			// to newRefs.
			if specificActorRef == refToRemove {
				found = true
				continue // Don't add to newRefs, effectively removing it.
			}
		}
		newRefs = append(newRefs, itemInSlice)
	}

	if !found {
		return false
	}

	// If the new list of references is empty, remove the key from the map.
	// Otherwise, update the map with the new slice.
	if len(newRefs) == 0 {
		delete(r.registrations, key.name)
	} else {
		r.registrations[key.name] = newRefs
	}

	return true
}

// ServiceKey is a type-safe identifier used for registering and discovering
// actors via the Receptionist. The generic type parameters M (Message) and R
// (Response) ensure that only actors handling compatible message/response types
// are associated with and retrieved for this key.
type ServiceKey[M Message, R any] struct {
	name string
}

// NewServiceKey creates a new service key with the given name. The name is used
// as the lookup key within the Receptionist.
func NewServiceKey[M Message, R any](name string) ServiceKey[M, R] {
	return ServiceKey[M, R]{name: name}
}

// Spawn registers an actor for this service key within the given ActorSystem.
// It's a convenience method that calls RegisterWithSystem, starting the actor
// and registering it with the receptionist.
func (sk ServiceKey[M, R]) Spawn(as *ActorSystem, id string,
	behavior ActorBehavior[M, R]) ActorRef[M, R] {

	return RegisterWithSystem(as, id, sk, behavior)
}

// Unregister removes an actor reference associated with this service key from
// the ActorSystem's receptionist and also stops the actor.
// It returns true if the actor was successfully unregistered from the
// receptionist AND successfully stopped and removed from the system's
// management. Otherwise, it returns false.
func (sk ServiceKey[M, R]) Unregister(as *ActorSystem,
	refToRemove ActorRef[M, R]) bool {

	unregisteredFromReceptionist := UnregisterFromReceptionist(
		as.Receptionist(), sk, refToRemove,
	)

	// If not found in receptionist, no need to try stopping.
	if !unregisteredFromReceptionist {
		return false
	}

	// Attempt to stop and remove the actor from the system.
	stoppedAndRemoved := as.StopAndRemoveActor(refToRemove.ID())

	return unregisteredFromReceptionist && stoppedAndRemoved
}

// UnregisterAll finds all actor references associated with this service key in
// the ActorSystem's receptionist. For each found actor, it attempts to stop it
// and remove it from system management, and also unregisters it from the
// receptionist.
func (sk ServiceKey[M, R]) UnregisterAll(as *ActorSystem) int {
	// First find all the refs that match this service key.
	refsFound := FindInReceptionist(as.Receptionist(), sk)

	actorsStoppedCount := 0
	for _, ref := range refsFound {
		// Attempt to stop and remove the actor from the system's active
		// management. This is the primary action to deactivate the
		// actor. If StopAndRemoveActor returns true, it means an active
		// actor was found in the system's `actors` map and was stopped.
		if as.StopAndRemoveActor(ref.ID()) {
			actorsStoppedCount++
		}

		// Regardless of whether the actor was actively managed by the
		// system (i.e., found in as.actors), attempt to unregister its
		// reference from the receptionist. This helps clean up any
		// potentially stale entries in the receptionist if an actor was
		// removed from the system's management without also being
		// unregistered from the receptionist.
		UnregisterFromReceptionist(as.Receptionist(), sk, ref)
	}

	return actorsStoppedCount
}

// Receptionist provides service discovery for actors. Actors can be registered
// under a ServiceKey and later discovered by other actors or system components.
type Receptionist struct {
	// registrations stores ActorRef instances, keyed by ServiceKey.name.
	registrations map[string][]any

	// mu protects access to registrations.
	mu sync.RWMutex
}

// newReceptionist creates a new Receptionist instance.
func newReceptionist() *Receptionist {
	return &Receptionist{
		registrations: make(map[string][]any),
	}
}

// RegisterWithReceptionist registers an actor with a service key in the given
// receptionist. This is a package-level generic function because methods
// cannot have their own type parameters in Go (as of the current version).
// It appends the actor reference to the list associated with the key's name.
func RegisterWithReceptionist[M Message, R any](
	r *Receptionist, key ServiceKey[M, R], ref ActorRef[M, R]) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Initialize the slice for this key if it's the first registration.
	if _, exists := r.registrations[key.name]; !exists {
		r.registrations[key.name] = make([]interface{}, 0)
	}

	r.registrations[key.name] = append(r.registrations[key.name], ref)
}

// FindInReceptionist returns all actors registered with a service key in the
// given receptionist. This is a package-level generic function because methods
// cannot have their own type parameters. It performs a type assertion to ensure
// that only ActorRefs matching the ServiceKey's generic types (M, R) are
// returned, providing type safety.
func FindInReceptionist[M Message, R any](
	r *Receptionist, key ServiceKey[M, R]) []ActorRef[M, R] {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if refs, exists := r.registrations[key.name]; exists {
		typedRefs := make([]ActorRef[M, R], 0, len(refs))
		for _, ref := range refs {
			// Make sure that the reference is of the correct type.
			// This type assertion is crucial for type safety, ensuring
			// that the returned ActorRefs match the expected M and R.
			if typedRef, ok := ref.(ActorRef[M, R]); ok {
				typedRefs = append(typedRefs, typedRef)
			}
		}
		return typedRefs
	}

	return nil
}
