<script lang="ts">
  // ConfirmationCard — inline card for a pending tool-call confirmation.
  //
  // Renders inline in the assistant turn, never as a modal. Shows approve/deny
  // buttons that post to the confirmations endpoint and record the decision in
  // the store. Counts down to expiresAt; greys out and disables buttons on
  // expiry (local clock) or on a confirmation.expired SSE event (state change).
  //
  // Tone:
  //   destructive / external risk tiers → error/clay surface (red-tinted tint)
  //   routine write tiers               → honey-bordered result style
  //
  // Security: entry.name is escaped interpolation (not {@html}). entry.args is
  // opaque unknown and rendered as a compact JSON string via plain interpolation —
  // never via {@html}. All text is safe through normal Svelte escaping.
  //
  // Accessibility: buttons are focusable/operable by keyboard. The card does NOT
  // steal focus on render (inline, not a modal). When expired, buttons carry
  // disabled and aria-disabled attributes.

  import { resolveConfirmation, confirmationExpired } from "$lib/stores/confirmations.svelte.js";
  import type { ConfirmationEntry, PendingConfirmation } from "$lib/stores/confirmations.svelte.js";
  import {
    confirmation_card_approve,
    confirmation_card_deny,
    confirmation_card_expired,
    confirmation_card_approved,
    confirmation_card_denied,
    confirmation_card_confirm_prompt,
    confirmation_card_expires_in,
  } from "$lib/paraglide/messages.js";

  // ---------------------------------------------------------------------------
  // Props
  // ---------------------------------------------------------------------------

  interface Props {
    entry: ConfirmationEntry;
  }

  const { entry }: Props = $props();

  // ---------------------------------------------------------------------------
  // Tone: destructive / external risk → clay/error; routine write → honey
  // ---------------------------------------------------------------------------

  /** Risk tiers that warrant the destructive (clay/error) visual tone. */
  const DESTRUCTIVE_RISKS = new Set(["destructive", "external"]);

  const isDestructive = $derived(DESTRUCTIVE_RISKS.has(entry.risk));

  // ---------------------------------------------------------------------------
  // Expiry countdown — $effect is appropriate: syncing with the system clock.
  // ---------------------------------------------------------------------------

  /** Seconds remaining until expiresAt, or 0 when expired. */
  let secondsRemaining = $state(0);

  function computeSecondsRemaining(): number {
    const expiresMs = new Date(entry.expiresAt).getTime();
    const remaining = Math.max(0, Math.floor((expiresMs - Date.now()) / 1000));
    return remaining;
  }

  $effect(() => {
    // Recompute whenever entry changes (expiresAt / state).
    if (entry.state !== "pending") {
      secondsRemaining = 0;
      return;
    }

    secondsRemaining = computeSecondsRemaining();
    if (secondsRemaining === 0) return; // already expired — no interval needed

    const id = setInterval(() => {
      const secs = computeSecondsRemaining();
      secondsRemaining = secs;
      if (secs === 0) {
        clearInterval(id);
        // Local clock expiry: transition store to expired so the component re-renders.
        confirmationExpired(entry.callId);
      }
    }, 1000);

    return () => {
      clearInterval(id);
    };
  });

  // ---------------------------------------------------------------------------
  // Disable condition: expired or resolved state, or local timer hit zero.
  // $derived keeps this in sync without a separate $effect.
  // ---------------------------------------------------------------------------

  const buttonsDisabled = $derived(entry.state !== "pending" || secondsRemaining === 0);

  // ---------------------------------------------------------------------------
  // Resolve action — POST to the confirmations endpoint
  // ---------------------------------------------------------------------------

  let resolving = $state(false);

  async function resolve(decision: "approve" | "deny"): Promise<void> {
    if (buttonsDisabled || resolving) return;
    // At this point buttonsDisabled is false, which requires entry.state === "pending".
    // The cast is safe: the guard above ensures we only reach here when pending.
    const pending = entry as PendingConfirmation;

    resolving = true;
    try {
      // resolveConfirmation posts to the server and guards the error before
      // recording the decision. A problem+json error (409/404/422) or network
      // throw leaves the card pending so the user can retry.
      await resolveConfirmation(pending, decision);
    } finally {
      resolving = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Compact args summary — JSON serialised, truncated, safe escaped interpolation.
  // ---------------------------------------------------------------------------

  function argsSummary(args: unknown): string {
    if (args === null || args === undefined) return "";
    try {
      const raw = JSON.stringify(args);
      if (raw === "{}" || raw === "[]" || raw === "") return "";
      // Trim long arg strings to avoid overwhelming the card.
      return raw.length > 120 ? raw.slice(0, 120) + "…" : raw;
    } catch {
      return "";
    }
  }

  const argsSummaryText = $derived(argsSummary(entry.args));
</script>

<div
  class="confirmation-card {isDestructive
    ? 'confirmation-card--destructive'
    : 'confirmation-card--routine'} {entry.state === 'expired' ? 'confirmation-card--expired' : ''}"
  role="group"
  aria-label={confirmation_card_confirm_prompt()}
>
  <!-- Header row: action name + expiry countdown -->
  <div class="confirmation-card__header">
    <span class="confirmation-card__name">{entry.name}</span>
    {#if entry.state === "pending" && secondsRemaining > 0}
      <span class="confirmation-card__timer" aria-live="off">
        {confirmation_card_expires_in({ seconds: secondsRemaining })}
      </span>
    {:else if entry.state === "expired"}
      <span class="confirmation-card__expired-label">{confirmation_card_expired()}</span>
    {:else if entry.state === "resolved"}
      <span class="confirmation-card__resolved-label">
        {entry.decision === "approve" ? confirmation_card_approved() : confirmation_card_denied()}
      </span>
    {/if}
  </div>

  <!-- Args summary (if any) — escaped interpolation, never {@html} -->
  {#if argsSummaryText !== ""}
    <p class="confirmation-card__args">{argsSummaryText}</p>
  {/if}

  <!-- Approve / Deny buttons — only rendered while pending or immediately after resolve -->
  {#if entry.state !== "expired"}
    <div class="confirmation-card__actions">
      <button
        type="button"
        class="confirmation-card__btn confirmation-card__btn--approve"
        disabled={buttonsDisabled || resolving}
        aria-disabled={buttonsDisabled || resolving}
        onclick={() => void resolve("approve")}
      >
        {confirmation_card_approve()}
      </button>
      <button
        type="button"
        class="confirmation-card__btn confirmation-card__btn--deny"
        disabled={buttonsDisabled || resolving}
        aria-disabled={buttonsDisabled || resolving}
        onclick={() => void resolve("deny")}
      >
        {confirmation_card_deny()}
      </button>
    </div>
  {/if}
</div>

<style>
  /* Base card: subtle inline block, not a modal. */
  .confirmation-card {
    display: inline-flex;
    flex-direction: column;
    gap: 0.375rem;
    border-radius: 0.5rem;
    padding: 0.625rem 0.875rem;
    font-size: 0.75rem;
    max-width: 100%;
    border-width: 1px;
    border-style: solid;
  }

  /* Destructive / external risk: clay/error surface (red-tinted tint). */
  .confirmation-card--destructive {
    background-color: var(--color-tint-error, #fbe6e1);
    border-color: var(--color-error, #cc4029);
    color: var(--color-fg-error, #a8331f);
  }

  /* Routine write: honey-bordered result style. */
  .confirmation-card--routine {
    background-color: var(--color-tint-warning, #fbebd2);
    border-color: var(--color-honey-500, #e0a458);
    color: var(--color-fg-warning, #855400);
  }

  /* Expired state: grey overlay regardless of tone. */
  .confirmation-card--expired {
    opacity: 0.5;
    filter: grayscale(0.6);
  }

  /* Header row: action name + status/timer. */
  .confirmation-card__header {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    flex-wrap: wrap;
  }

  .confirmation-card__name {
    font-weight: 600;
    font-family: var(--font-mono, ui-monospace, monospace);
    font-size: 0.7rem;
  }

  .confirmation-card__timer {
    margin-left: auto;
    font-size: 0.65rem;
    opacity: 0.75;
  }

  .confirmation-card__expired-label {
    margin-left: auto;
    font-size: 0.65rem;
    font-weight: 500;
  }

  .confirmation-card__resolved-label {
    margin-left: auto;
    font-size: 0.65rem;
    font-weight: 500;
  }

  /* Args: monospace compact, truncated. */
  .confirmation-card__args {
    margin: 0;
    font-family: var(--font-mono, ui-monospace, monospace);
    font-size: 0.65rem;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 40ch;
    opacity: 0.8;
  }

  /* Action row. */
  .confirmation-card__actions {
    display: flex;
    gap: 0.5rem;
    margin-top: 0.125rem;
  }

  /* Buttons — base. */
  .confirmation-card__btn {
    cursor: pointer;
    border-radius: 0.375rem;
    padding: 0.25rem 0.75rem;
    font-size: 0.7rem;
    font-weight: 500;
    border-width: 1px;
    border-style: solid;
    transition: opacity 120ms ease;
  }

  .confirmation-card__btn:focus-visible {
    outline: 2px solid currentColor;
    outline-offset: 2px;
  }

  .confirmation-card__btn:disabled,
  .confirmation-card__btn[aria-disabled="true"] {
    cursor: not-allowed;
    opacity: 0.4;
  }

  /* Approve button: inherits card tone for the background accent. */
  .confirmation-card--destructive .confirmation-card__btn--approve {
    background-color: var(--color-error, #cc4029);
    border-color: var(--color-error, #cc4029);
    color: #fff;
  }

  .confirmation-card--destructive .confirmation-card__btn--approve:hover:not(:disabled) {
    opacity: 0.85;
  }

  .confirmation-card--routine .confirmation-card__btn--approve {
    background-color: var(--color-honey-500, #e0a458);
    border-color: var(--color-honey-600, #c9883c);
    color: #fff;
  }

  .confirmation-card--routine .confirmation-card__btn--approve:hover:not(:disabled) {
    background-color: var(--color-honey-600, #c9883c);
  }

  /* Deny button: ghost style, same tone. */
  .confirmation-card__btn--deny {
    background-color: transparent;
  }

  .confirmation-card--destructive .confirmation-card__btn--deny {
    border-color: var(--color-error, #cc4029);
    color: var(--color-fg-error, #a8331f);
  }

  .confirmation-card--destructive .confirmation-card__btn--deny:hover:not(:disabled) {
    background-color: color-mix(in srgb, var(--color-error, #cc4029) 10%, transparent);
  }

  .confirmation-card--routine .confirmation-card__btn--deny {
    border-color: var(--color-honey-500, #e0a458);
    color: var(--color-fg-warning, #855400);
  }

  .confirmation-card--routine .confirmation-card__btn--deny:hover:not(:disabled) {
    background-color: color-mix(in srgb, var(--color-honey-500, #e0a458) 10%, transparent);
  }
</style>
