#include "cancel_token.h"

#include <array>
#include <cstddef>
#include <cstdint>
#include <limits>

#include "error.h"

namespace vc::detail {
namespace {

constexpr size_t kCancelSlotCount = 4096u;
constexpr uintptr_t kSlotMask = 0xffffu;
constexpr unsigned kGenerationShift = 16u;
constexpr uint32_t kMaximumGeneration = 0x7fffffffu;

enum class SlotState : uint64_t {
    free = 0u,
    initializing = 1u,
    active = 2u,
    disposed = 3u,
};

constexpr uint64_t kStateMask = 0x3u;

struct CancelSlot {
    std::atomic<uint64_t> control{0u};
    std::atomic<uint32_t> references{0u};
    std::atomic<uint32_t> requested{0u};
};

struct DecodedHandle {
    CancelSlot* slot;
    uint32_t generation;
};

std::array<CancelSlot, kCancelSlotCount> cancel_slots;
std::atomic<uint32_t> next_slot{0u};

#if defined(VC_CANCEL_TESTING)
CancelTestHook cancel_test_hook = nullptr;
void* cancel_test_hook_context = nullptr;
#endif

constexpr uint64_t MakeControl(uint32_t generation,
                               SlotState state) noexcept {
    return (static_cast<uint64_t>(generation) << 2u) |
           static_cast<uint64_t>(state);
}

constexpr uint32_t ControlGeneration(uint64_t control) noexcept {
    return static_cast<uint32_t>(control >> 2u);
}

constexpr SlotState ControlState(uint64_t control) noexcept {
    return static_cast<SlotState>(control & kStateMask);
}

template <typename Handle>
Handle* EncodeHandle(size_t slot_index,
                     uint32_t generation) noexcept {
    const uintptr_t value =
        (static_cast<uintptr_t>(generation) << kGenerationShift) |
        static_cast<uintptr_t>(slot_index + 1u);
    return reinterpret_cast<Handle*>(value);
}

template <typename Handle>
DecodedHandle DecodeHandle(const Handle* handle) noexcept {
    const uintptr_t value = reinterpret_cast<uintptr_t>(handle);
    const uintptr_t encoded_slot = value & kSlotMask;
    const uintptr_t encoded_generation =
        value >> kGenerationShift;
    if (encoded_slot == 0u ||
        encoded_slot > kCancelSlotCount ||
        encoded_generation == 0u ||
        encoded_generation > kMaximumGeneration) {
        return {nullptr, 0u};
    }
    return {
        &cancel_slots[static_cast<size_t>(encoded_slot - 1u)],
        static_cast<uint32_t>(encoded_generation),
    };
}

size_t SlotIndex(const CancelSlot* slot) noexcept {
    return static_cast<size_t>(slot - cancel_slots.data());
}

void RunPinAcquiredHook(CancelSlot* slot,
                        uint32_t generation) noexcept {
#if defined(VC_CANCEL_TESTING)
    if (cancel_test_hook != nullptr) {
        cancel_test_hook(CancelTestEvent::pin_acquired,
                         SlotIndex(slot),
                         generation,
                         cancel_test_hook_context);
    }
#else
    (void)slot;
    (void)generation;
#endif
}

void RunSlotReleasedHook(CancelSlot* slot,
                         uint32_t generation) noexcept {
#if defined(VC_CANCEL_TESTING)
    if (cancel_test_hook != nullptr) {
        cancel_test_hook(CancelTestEvent::slot_released,
                         SlotIndex(slot),
                         generation,
                         cancel_test_hook_context);
    }
#else
    (void)slot;
    (void)generation;
#endif
}

void ReleaseReference(CancelSlot* slot,
                      uint32_t generation) noexcept {
    if (slot == nullptr) {
        return;
    }

    uint64_t control = slot->control.load(std::memory_order_acquire);
    if (ControlGeneration(control) != generation) {
        return;
    }

    uint32_t references =
        slot->references.load(std::memory_order_acquire);
    while (references != 0u) {
        if (slot->references.compare_exchange_weak(
                references,
                references - 1u,
                std::memory_order_acq_rel,
                std::memory_order_acquire)) {
            if (references != 1u) {
                return;
            }

            uint64_t disposed =
                MakeControl(generation, SlotState::disposed);
            if (slot->control.compare_exchange_strong(
                    disposed,
                    MakeControl(generation, SlotState::free),
                    std::memory_order_release,
                    std::memory_order_relaxed)) {
                RunSlotReleasedHook(slot, generation);
            }
            return;
        }
    }
}

CancelSlot* PinActive(const DecodedHandle& decoded) noexcept {
    if (decoded.slot == nullptr) {
        return nullptr;
    }
    const uint64_t expected_control =
        MakeControl(decoded.generation, SlotState::active);
    if (decoded.slot->control.load(std::memory_order_acquire) !=
        expected_control) {
        return nullptr;
    }
    if (!TryRetainReference(decoded.slot->references)) {
        return nullptr;
    }
    RunPinAcquiredHook(decoded.slot, decoded.generation);

    const uint64_t after =
        decoded.slot->control.load(std::memory_order_acquire);
    if (after == expected_control) {
        return decoded.slot;
    }
    ReleaseReference(decoded.slot, ControlGeneration(after));
    return nullptr;
}

CancelSlot* PinRetainedState(
    const DecodedHandle& decoded) noexcept {
    if (decoded.slot == nullptr) {
        return nullptr;
    }
    uint64_t control =
        decoded.slot->control.load(std::memory_order_acquire);
    if (ControlGeneration(control) != decoded.generation ||
        (ControlState(control) != SlotState::active &&
         ControlState(control) != SlotState::disposed)) {
        return nullptr;
    }
    if (!TryRetainReference(decoded.slot->references)) {
        return nullptr;
    }

    control = decoded.slot->control.load(std::memory_order_acquire);
    if (ControlGeneration(control) == decoded.generation &&
        (ControlState(control) == SlotState::active ||
         ControlState(control) == SlotState::disposed)) {
        return decoded.slot;
    }
    ReleaseReference(decoded.slot, ControlGeneration(control));
    return nullptr;
}

}  // namespace

static_assert(sizeof(uintptr_t) >= sizeof(uint64_t),
              "encoded cancel handles require a 64-bit target");
static_assert(std::atomic<uint64_t>::is_always_lock_free,
              "cancel slot control requires lock-free uint64 atomics");

int32_t CreateCancelToken(vc_cancel_token** out,
                          vc_error* error) {
    const uint32_t start =
        next_slot.fetch_add(1u, std::memory_order_relaxed);
    for (size_t offset = 0; offset < kCancelSlotCount; ++offset) {
        const size_t index =
            (static_cast<size_t>(start) + offset) %
            kCancelSlotCount;
        CancelSlot& slot = cancel_slots[index];
        uint64_t control =
            slot.control.load(std::memory_order_acquire);
        if (ControlState(control) != SlotState::free) {
            continue;
        }
        const uint32_t old_generation =
            ControlGeneration(control);
        if (old_generation == kMaximumGeneration) {
            continue;
        }
        const uint32_t generation = old_generation + 1u;
        const uint64_t initializing =
            MakeControl(generation, SlotState::initializing);
        if (!slot.control.compare_exchange_strong(
                control,
                initializing,
                std::memory_order_acq_rel,
                std::memory_order_acquire)) {
            continue;
        }

        slot.requested.store(0u, std::memory_order_relaxed);
        slot.references.store(1u, std::memory_order_relaxed);
        slot.control.store(
            MakeControl(generation, SlotState::active),
            std::memory_order_release);
        *out = EncodeHandle<vc_cancel_token>(index, generation);
        SetError(error, VC_OK, 0, 0, "");
        return VC_OK;
    }

    SetError(error,
             VC_ERR_OOM,
             0,
             0,
             "cancel token capacity exhausted");
    return VC_ERR_OOM;
}

void RequestCancel(vc_cancel_token* token) noexcept {
    const DecodedHandle decoded = DecodeHandle(token);
    CancelSlot* const slot = PinActive(decoded);
    if (slot != nullptr) {
        slot->requested.store(1u, std::memory_order_release);
        ReleaseReference(slot, decoded.generation);
    }
}

void FreeCancelToken(vc_cancel_token* token) noexcept {
    const DecodedHandle decoded = DecodeHandle(token);
    if (decoded.slot == nullptr) {
        return;
    }
    uint64_t active =
        MakeControl(decoded.generation, SlotState::active);
    if (decoded.slot->control.compare_exchange_strong(
            active,
            MakeControl(decoded.generation, SlotState::disposed),
            std::memory_order_acq_rel,
            std::memory_order_acquire)) {
        ReleaseReference(decoded.slot, decoded.generation);
    }
}

CancelState* RetainCancelState(vc_cancel_token* token) noexcept {
    const DecodedHandle decoded = DecodeHandle(token);
    if (PinActive(decoded) == nullptr) {
        return nullptr;
    }
    return EncodeHandle<CancelState>(
        static_cast<size_t>(decoded.slot - cancel_slots.data()),
        decoded.generation);
}

void ReleaseCancelState(CancelState* state) noexcept {
    const DecodedHandle decoded = DecodeHandle(state);
    ReleaseReference(decoded.slot, decoded.generation);
}

bool IsCancellationRequested(const CancelState* state) noexcept {
    const DecodedHandle decoded = DecodeHandle(state);
    CancelSlot* const slot = PinRetainedState(decoded);
    if (slot == nullptr) {
        return false;
    }
    const bool requested =
        slot->requested.load(std::memory_order_acquire) != 0u;
    ReleaseReference(slot, decoded.generation);
    return requested;
}

bool TryRetainReference(
    std::atomic<uint32_t>& references) noexcept {
    uint32_t value = references.load(std::memory_order_acquire);
    while (value != 0u &&
           value != (std::numeric_limits<uint32_t>::max)()) {
        if (references.compare_exchange_weak(
                value,
                value + 1u,
                std::memory_order_acq_rel,
                std::memory_order_acquire)) {
            return true;
        }
    }
    return false;
}

#if defined(VC_CANCEL_TESTING)
void CancelTestReset() noexcept {
    cancel_test_hook = nullptr;
    cancel_test_hook_context = nullptr;
    next_slot.store(0u, std::memory_order_relaxed);
    for (CancelSlot& slot : cancel_slots) {
        slot.requested.store(0u, std::memory_order_relaxed);
        slot.references.store(0u, std::memory_order_relaxed);
        slot.control.store(0u, std::memory_order_relaxed);
    }
}

size_t CancelTestSlotCapacity() noexcept {
    return kCancelSlotCount;
}

uint32_t CancelTestMaximumGeneration() noexcept {
    return kMaximumGeneration;
}

void CancelTestSetNextSlot(size_t slot_index) noexcept {
    next_slot.store(
        static_cast<uint32_t>(slot_index % kCancelSlotCount),
        std::memory_order_relaxed);
}

bool CancelTestSeedFreeSlot(size_t slot_index,
                            uint32_t generation) noexcept {
    if (slot_index >= kCancelSlotCount ||
        generation > kMaximumGeneration) {
        return false;
    }
    CancelSlot& slot = cancel_slots[slot_index];
    if (slot.references.load(std::memory_order_acquire) != 0u ||
        ControlState(slot.control.load(std::memory_order_acquire)) !=
            SlotState::free) {
        return false;
    }
    slot.requested.store(0u, std::memory_order_relaxed);
    slot.control.store(
        MakeControl(generation, SlotState::free),
        std::memory_order_release);
    return true;
}

size_t CancelTestHandleSlot(
    const vc_cancel_token* token) noexcept {
    const DecodedHandle decoded = DecodeHandle(token);
    return decoded.slot == nullptr
               ? kCancelSlotCount
               : SlotIndex(decoded.slot);
}

uint32_t CancelTestHandleGeneration(
    const vc_cancel_token* token) noexcept {
    return DecodeHandle(token).generation;
}

uint32_t CancelTestSlotGeneration(
    size_t slot_index) noexcept {
    if (slot_index >= kCancelSlotCount) {
        return 0u;
    }
    return ControlGeneration(
        cancel_slots[slot_index].control.load(
            std::memory_order_acquire));
}

bool CancelTestSlotIsFree(size_t slot_index) noexcept {
    return slot_index < kCancelSlotCount &&
           ControlState(cancel_slots[slot_index].control.load(
               std::memory_order_acquire)) == SlotState::free &&
           cancel_slots[slot_index].references.load(
               std::memory_order_acquire) == 0u;
}

void CancelTestSetHook(CancelTestHook hook,
                       void* context) noexcept {
    cancel_test_hook = hook;
    cancel_test_hook_context = context;
}
#endif

}  // namespace vc::detail
