#ifndef VIDEOCORE_SRC_CANCEL_TOKEN_H
#define VIDEOCORE_SRC_CANCEL_TOKEN_H

#include <atomic>
#include <cstddef>
#include <cstdint>

#include "videocore/videocore.h"

namespace vc::detail {

struct CancelState;

static_assert(std::atomic<uint32_t>::is_always_lock_free,
              "cancel state requires lock-free uint32 atomics");

int32_t CreateCancelToken(vc_cancel_token** out,
                          vc_error* error);
void RequestCancel(vc_cancel_token* token) noexcept;
void FreeCancelToken(vc_cancel_token* token) noexcept;

CancelState* RetainCancelState(vc_cancel_token* token) noexcept;
void ReleaseCancelState(CancelState* state) noexcept;
bool IsCancellationRequested(const CancelState* state) noexcept;
bool TryRetainReference(
    std::atomic<uint32_t>& references) noexcept;

#if defined(VC_CANCEL_TESTING)
enum class CancelTestEvent : uint32_t {
    pin_acquired = 1u,
    slot_released = 2u,
};

using CancelTestHook = void (*)(
    CancelTestEvent event,
    size_t slot_index,
    uint32_t generation,
    void* context) noexcept;

void CancelTestReset() noexcept;
size_t CancelTestSlotCapacity() noexcept;
uint32_t CancelTestMaximumGeneration() noexcept;
void CancelTestSetNextSlot(size_t slot_index) noexcept;
bool CancelTestSeedFreeSlot(size_t slot_index,
                            uint32_t generation) noexcept;
size_t CancelTestHandleSlot(
    const vc_cancel_token* token) noexcept;
uint32_t CancelTestHandleGeneration(
    const vc_cancel_token* token) noexcept;
uint32_t CancelTestSlotGeneration(
    size_t slot_index) noexcept;
bool CancelTestSlotIsFree(size_t slot_index) noexcept;
void CancelTestSetHook(CancelTestHook hook,
                       void* context) noexcept;
#endif

}  // namespace vc::detail

#endif
