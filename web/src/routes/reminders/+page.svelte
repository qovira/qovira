<script lang="ts">
  // Reminders page — grouped chronological view with live SSE updates.
  //
  // Renders four time buckets (Overdue · Today · This week · Later) plus a
  // "Done" section loaded on demand. Live updates fall out for free because all
  // derives are over the module-level $state in the reminders store, which the
  // SSE client already patches on every reminder.* event.
  //
  // Bucket boundary choice (see also src/lib/reminders/bucket.ts):
  //   Overdue:    dueAt < now
  //   Today:      now <= dueAt < next local midnight
  //   This week:  next local midnight <= dueAt < next Monday 00:00 local (ISO week)
  //   Later:      dueAt >= next Monday 00:00 local
  //   Done:       status === "completed", loaded on demand via GET /reminders?status=completed
  //
  // Security: reminder.title flows through {reminder.title} — Svelte escapes it
  // automatically. Never use {@html} for user-supplied content.

  import { ulid } from "ulid";
  import { toast } from "@qovira/ui";
  import { Button, Field, Input } from "@qovira/ui";
  import { Api } from "$lib/api/index.js";
  import { getReminders, upsertReminder } from "$lib/stores/reminders.svelte.js";
  import type { ReminderItem } from "$lib/stores/reminders.svelte.js";
  import { bucketReminders, shouldShowPlaceholder } from "$lib/reminders/bucket.js";
  import {
    applyOptimisticCreate,
    confirmCreate,
    revertCreate,
    applyOptimisticComplete,
    confirmComplete,
    revertComplete,
    buildNextDueAt,
    defaultDueAtLocal,
    dueAtToLocal,
    rruleToPreset,
    makeReminderPatchBody,
    type RrulePreset,
  } from "$lib/reminders/actions.svelte.js";
  import SlideOver from "$lib/components/SlideOver.svelte";
  import ReminderRow from "$lib/components/ReminderRow.svelte";
  import {
    reminders_placeholder,
    reminders_bucket_overdue,
    reminders_bucket_today,
    reminders_bucket_this_week,
    reminders_bucket_later,
    reminders_bucket_done,
    reminders_done_toggle_label,
    reminders_done_loading,
    reminders_done_empty,
    reminders_done_load_error,
    reminders_quick_add_title_placeholder,
    reminders_quick_add_due_label,
    reminders_quick_add_submit,
    reminders_quick_add_error,
    reminders_complete_error,
    reminders_edit_title,
    reminders_edit_field_title,
    reminders_edit_field_due,
    reminders_edit_field_recurrence,
    reminders_edit_field_notes,
    reminders_edit_recurrence_none,
    reminders_edit_recurrence_daily,
    reminders_edit_recurrence_weekly,
    reminders_edit_recurrence_monthly,
    reminders_edit_recurrence_keep,
    reminders_edit_save,
    reminders_edit_saving,
    reminders_edit_error,
  } from "$lib/paraglide/messages.js";

  // ---------------------------------------------------------------------------
  // "now" — refreshed on a coarse 30-second timer so bucket boundaries
  // (overdue / today / this-week, midnight crossings) stay current while the
  // route stays mounted. Minute granularity is sufficient; 30s is a safe
  // midpoint between responsiveness and CPU cost.
  // $effect is appropriate: syncing with a browser timer (external system).
  // ---------------------------------------------------------------------------
  let now = $state(new Date());

  $effect(() => {
    const id = setInterval(() => {
      now = new Date();
    }, 30_000);
    return () => clearInterval(id);
  });

  // ---------------------------------------------------------------------------
  // Bucketed views — derived from the live store, recompute on every mutation.
  // ---------------------------------------------------------------------------
  const buckets = $derived(bucketReminders(getReminders(), now));

  const overdue = $derived(buckets.overdue);
  const today = $derived(buckets.today);
  const thisWeek = $derived(buckets.thisWeek);
  const later = $derived(buckets.later);
  const done = $derived(buckets.done);

  const hasNoActive = $derived(
    overdue.length === 0 && today.length === 0 && thisWeek.length === 0 && later.length === 0,
  );

  const showPlaceholder = $derived(shouldShowPlaceholder(hasNoActive, done.length));

  // ---------------------------------------------------------------------------
  // Done section — loaded on demand (disclosure pattern).
  // ---------------------------------------------------------------------------

  let doneOpen = $state(false);
  let doneLoading = $state(false);
  let doneError = $state<string | null>(null);
  let doneFetched = $state(false);

  async function openDone(): Promise<void> {
    doneOpen = true;
    if (doneFetched || doneLoading) return;

    doneLoading = true;
    doneError = null;

    try {
      let cursor: string | null = null;

      for (;;) {
        const query: { status: "completed"; cursor?: string } =
          cursor !== null ? { status: "completed", cursor } : { status: "completed" };
        const result = await Api.GET("/reminders", { params: { query } });
        const page = result.data;

        if (page?.data !== undefined) {
          for (const r of page.data) {
            upsertReminder(r);
          }
        }

        const nextCursor: string | null = page?.pagination.nextCursor ?? null;
        if (nextCursor !== null && nextCursor !== "") {
          cursor = nextCursor;
        } else {
          break;
        }
      }
    } catch {
      doneError = reminders_done_load_error();
    } finally {
      doneLoading = false;
      doneFetched = true;
    }
  }

  function toggleDone(): void {
    if (!doneOpen) {
      void openDone();
    } else {
      doneOpen = false;
    }
  }

  // ---------------------------------------------------------------------------
  // 1. Quick-add (optimistic)
  // ---------------------------------------------------------------------------

  let quickAddTitle = $state("");
  let quickAddDueAt = $state(defaultDueAtLocal());
  let quickAddSubmitting = $state(false);

  async function handleQuickAdd(e: SubmitEvent): Promise<void> {
    e.preventDefault();
    const title = quickAddTitle.trim();
    const dueAtRfc = buildNextDueAt(quickAddDueAt);
    if (!title || !dueAtRfc) return;
    if (quickAddSubmitting) return;

    quickAddSubmitting = true;

    const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
    const nowIso = new Date().toISOString();
    const tempId = ulid();
    const temp: ReminderItem = {
      id: tempId,
      userId: "",
      title,
      dueAt: dueAtRfc,
      tz,
      autoComplete: true,
      status: "active",
      createdAt: nowIso,
      updatedAt: nowIso,
    };

    applyOptimisticCreate(temp);
    // Reset the title immediately so the user can start typing the next one.
    quickAddTitle = "";

    try {
      const result = await Api.POST("/reminders", {
        body: { title, dueAt: dueAtRfc, tz, autoComplete: true },
      });

      if (result.error !== undefined) {
        // Server rejected — revert the optimistic item (AC5).
        revertCreate(tempId);
        toast.error(reminders_quick_add_error());
      } else {
        // Success — swap temp for the real reminder.
        confirmCreate(tempId, result.data);
        // Reset due-at to next-hour default for the next quick-add.
        quickAddDueAt = defaultDueAtLocal();
      }
    } catch {
      revertCreate(tempId);
      toast.error(reminders_quick_add_error());
    } finally {
      quickAddSubmitting = false;
    }
  }

  // ---------------------------------------------------------------------------
  // 2. Complete (optimistic)
  // ---------------------------------------------------------------------------

  async function handleComplete(id: string): Promise<void> {
    const snapshot = applyOptimisticComplete(id);
    if (snapshot === null) return; // reminder not in store — no-op

    try {
      const result = await Api.PATCH("/reminders/{id}", {
        params: { path: { id } },
        body: { status: "completed" },
      });

      if (result.error !== undefined) {
        // Server rejected — revert to the prior state (AC5).
        revertComplete(snapshot);
        toast.error(reminders_complete_error());
      } else {
        confirmComplete(result.data);
      }
    } catch {
      revertComplete(snapshot);
      toast.error(reminders_complete_error());
    }
  }

  // ---------------------------------------------------------------------------
  // 3. Edit sheet (non-optimistic)
  // ---------------------------------------------------------------------------

  let editOpen = $state(false);
  let editReminder = $state<ReminderItem | null>(null);

  // Edit form fields (local state, not committed to the store until save succeeds).
  let editTitle = $state("");
  let editDueAt = $state(""); // datetime-local string
  let editNotes = $state("");
  let editRrulePreset = $state<RrulePreset>("none");
  let editSaving = $state(false);
  let editError = $state<string | null>(null);

  function openEdit(reminder: ReminderItem): void {
    editReminder = reminder;
    editTitle = reminder.title;
    editDueAt = dueAtToLocal(reminder.dueAt);
    editNotes = reminder.notes ?? "";
    editRrulePreset = rruleToPreset(reminder.rrule);
    editError = null;
    editSaving = false;
    editOpen = true;
  }

  function closeEdit(): void {
    editOpen = false;
    editReminder = null;
    editError = null;
  }

  async function handleEditSave(e: SubmitEvent): Promise<void> {
    e.preventDefault();
    const r = editReminder;
    if (r === null || editSaving) return;

    const dueAtRfc = buildNextDueAt(editDueAt);
    if (!editTitle.trim() || !dueAtRfc) return;

    editSaving = true;
    editError = null;

    const patchBody = makeReminderPatchBody(r, {
      title: editTitle.trim(),
      dueAt: dueAtRfc,
      notes: editNotes,
      rrulePreset: editRrulePreset,
    });

    try {
      const result = await Api.PATCH("/reminders/{id}", {
        params: { path: { id: r.id } },
        body: patchBody,
      });

      if (result.error !== undefined) {
        // Non-optimistic: do NOT mutate store. Keep sheet open, show inline error.
        editError = reminders_edit_error();
      } else {
        // Success: update the store, close the sheet (AC3, AC4).
        upsertReminder(result.data);
        closeEdit();
      }
    } catch {
      editError = reminders_edit_error();
    } finally {
      editSaving = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Helper: clear a stale inline error when the user edits any edit-sheet field.
  // Called from each field's input/change handler so the error banner disappears
  // the moment they start correcting — not just on the next submit.
  // ---------------------------------------------------------------------------
  function clearEditError(): void {
    editError = null;
  }

  // ---------------------------------------------------------------------------
  // Helper: is the rrule preset a known match-able preset (not "keep")?
  // Used to guard the select value for the "keep" option visibility.
  // ---------------------------------------------------------------------------
  const PRESET_OPTIONS: RrulePreset[] = ["none", "daily", "weekly", "monthly"];
</script>

<!--
  Single-column reminders view.
  Quick-add form is pinned at the top, above the buckets.
-->

<!-- =========================================================================
  1. Quick-add form — pinned above buckets, compact one-row layout.
  Uses native form submission (Enter-to-submit) + the ui Button.
  The due date/time uses a themed native datetime-local input.
  ========================================================================= -->
<form class="reminder-quick-add" onsubmit={(e) => void handleQuickAdd(e)}>
  <input
    type="text"
    class="reminder-quick-add__title"
    placeholder={reminders_quick_add_title_placeholder()}
    bind:value={quickAddTitle}
    disabled={quickAddSubmitting}
    required
    aria-label={reminders_quick_add_title_placeholder()}
  />
  <label class="reminder-quick-add__due-label" for="quick-add-due">
    {reminders_quick_add_due_label()}
  </label>
  <input
    id="quick-add-due"
    type="datetime-local"
    class="reminder-quick-add__due"
    bind:value={quickAddDueAt}
    disabled={quickAddSubmitting}
    required
    aria-label={reminders_quick_add_due_label()}
  />
  <Button type="submit" variant="primary" disabled={quickAddSubmitting} loading={quickAddSubmitting}>
    {reminders_quick_add_submit()}
  </Button>
</form>

<!-- =========================================================================
  Bucketed reminder sections.
  A11y: each row is restructured as SIBLING buttons (check-circle + row body)
  inside the <li>, NOT nested buttons. This satisfies the interactive-child
  constraint and is valid HTML / a11y.
  ========================================================================= -->

<div class="reminders-page">
  {#if showPlaceholder}
    <p class="text-fg-muted text-sm">{reminders_placeholder()}</p>
  {:else if hasNoActive}
    <!-- Zero active but some done — no active buckets; Done section below shows them. -->
  {:else}
    <!-- Overdue -->
    {#if overdue.length > 0}
      <section class="reminders-bucket" aria-labelledby="bucket-overdue">
        <h2 id="bucket-overdue" class="reminders-bucket__heading reminders-bucket__heading--overdue">
          {reminders_bucket_overdue()}
        </h2>
        <ul class="reminders-list" role="list">
          {#each overdue as reminder (reminder.id)}
            <ReminderRow {reminder} oncomplete={(id: string) => void handleComplete(id)} onedit={openEdit} />
          {/each}
        </ul>
      </section>
    {/if}

    <!-- Today -->
    {#if today.length > 0}
      <section class="reminders-bucket" aria-labelledby="bucket-today">
        <h2 id="bucket-today" class="reminders-bucket__heading">
          {reminders_bucket_today()}
        </h2>
        <ul class="reminders-list" role="list">
          {#each today as reminder (reminder.id)}
            <ReminderRow {reminder} oncomplete={(id: string) => void handleComplete(id)} onedit={openEdit} />
          {/each}
        </ul>
      </section>
    {/if}

    <!-- This week -->
    {#if thisWeek.length > 0}
      <section class="reminders-bucket" aria-labelledby="bucket-this-week">
        <h2 id="bucket-this-week" class="reminders-bucket__heading">
          {reminders_bucket_this_week()}
        </h2>
        <ul class="reminders-list" role="list">
          {#each thisWeek as reminder (reminder.id)}
            <ReminderRow {reminder} oncomplete={(id: string) => void handleComplete(id)} onedit={openEdit} />
          {/each}
        </ul>
      </section>
    {/if}

    <!-- Later -->
    {#if later.length > 0}
      <section class="reminders-bucket" aria-labelledby="bucket-later">
        <h2 id="bucket-later" class="reminders-bucket__heading">
          {reminders_bucket_later()}
        </h2>
        <ul class="reminders-list" role="list">
          {#each later as reminder (reminder.id)}
            <ReminderRow {reminder} oncomplete={(id: string) => void handleComplete(id)} onedit={openEdit} />
          {/each}
        </ul>
      </section>
    {/if}
  {/if}

  <!-- Done section — always available, opened on demand -->
  <section class="reminders-bucket reminders-bucket--done" aria-labelledby="bucket-done">
    <button
      type="button"
      class="reminders-done-toggle"
      onclick={toggleDone}
      aria-expanded={doneOpen}
      aria-label={reminders_done_toggle_label()}
    >
      <h2 id="bucket-done" class="reminders-bucket__heading">
        {reminders_bucket_done()}
        {#if done.length > 0}
          <span class="reminders-done-count" aria-hidden="true">({done.length})</span>
        {/if}
      </h2>
      <!-- Chevron indicator — down when open, right when closed -->
      <svg
        class="reminders-done-chevron {doneOpen ? 'reminders-done-chevron--open' : ''}"
        xmlns="http://www.w3.org/2000/svg"
        width="14"
        height="14"
        viewBox="0 0 256 256"
        aria-hidden="true"
      >
        <path
          fill="currentColor"
          d="m213.66 101.66-80 80a8 8 0 0 1-11.32 0l-80-80A8 8 0 0 1 53.66 90.34L128 164.69l74.34-74.35a8 8 0 0 1 11.32 11.32Z"
        />
      </svg>
    </button>

    {#if doneOpen}
      {#if doneLoading}
        <p class="text-fg-muted text-sm" role="status" aria-live="polite">{reminders_done_loading()}</p>
      {:else if doneError !== null}
        <p class="text-fg-error text-sm" role="alert">{doneError}</p>
      {:else if done.length === 0}
        <p class="text-fg-muted text-sm">{reminders_done_empty()}</p>
      {:else}
        <ul class="reminders-list" role="list">
          {#each done as reminder (reminder.id)}
            <ReminderRow {reminder} isDone={true} onedit={openEdit} />
          {/each}
        </ul>
      {/if}
    {/if}
  </section>
</div>

<!-- =========================================================================
  3. Edit slide-over sheet (transient, non-optimistic).
  Uses the existing SlideOver component (native <dialog>, accessible).
  ========================================================================= -->
<SlideOver bind:open={editOpen} title={reminders_edit_title()} onclose={closeEdit}>
  {#if editReminder !== null}
    <form class="edit-sheet-form" onsubmit={(e) => void handleEditSave(e)}>
      <!-- Title — Input reads Field's accessibility contract from context automatically. -->
      <Field label={reminders_edit_field_title()}>
        {#snippet children()}
          <Input
            type="text"
            value={editTitle}
            oninput={(e: Event & { currentTarget: HTMLInputElement }) => {
              editTitle = e.currentTarget.value;
              clearEditError();
            }}
            disabled={editSaving}
            required
          />
        {/snippet}
      </Field>

      <!-- Due date/time — native input; reads id/describedby from FieldContext arg. -->
      <Field label={reminders_edit_field_due()}>
        {#snippet children({ id, describedby })}
          <input
            {id}
            aria-describedby={describedby}
            type="datetime-local"
            class="edit-sheet-datetime"
            bind:value={editDueAt}
            oninput={clearEditError}
            disabled={editSaving}
            required
          />
        {/snippet}
      </Field>

      <!-- Recurrence preset select — native select; reads id from FieldContext. -->
      <Field label={reminders_edit_field_recurrence()}>
        {#snippet children({ id, describedby })}
          <select
            {id}
            aria-describedby={describedby}
            class="edit-sheet-select"
            bind:value={editRrulePreset}
            onchange={clearEditError}
            disabled={editSaving}
          >
            {#each PRESET_OPTIONS as preset (preset)}
              <option value={preset}>
                {#if preset === "none"}
                  {reminders_edit_recurrence_none()}
                {:else if preset === "daily"}
                  {reminders_edit_recurrence_daily()}
                {:else if preset === "weekly"}
                  {reminders_edit_recurrence_weekly()}
                {:else}
                  {reminders_edit_recurrence_monthly()}
                {/if}
              </option>
            {/each}
            <!--
              "Keep current schedule" option only shown when the existing rrule
              does not match a known preset (e.g. FREQ=WEEKLY;COUNT=3).
              This preserves unknown rrules rather than silently dropping them.
            -->
            {#if editRrulePreset === "keep"}
              <option value="keep">{reminders_edit_recurrence_keep()}</option>
            {/if}
          </select>
        {/snippet}
      </Field>

      <!-- Notes — Input reads Field context automatically. -->
      <Field label={reminders_edit_field_notes()}>
        {#snippet children()}
          <Input
            type="text"
            value={editNotes}
            oninput={(e: Event & { currentTarget: HTMLInputElement }) => {
              editNotes = e.currentTarget.value;
              clearEditError();
            }}
            disabled={editSaving}
          />
        {/snippet}
      </Field>

      <!-- Inline error -->
      {#if editError !== null}
        <p class="edit-sheet-error" role="alert">{editError}</p>
      {/if}

      <div class="edit-sheet-footer">
        <Button type="submit" variant="primary" disabled={editSaving} loading={editSaving}>
          {editSaving ? reminders_edit_saving() : reminders_edit_save()}
        </Button>
      </div>
    </form>
  {/if}
</SlideOver>

<style>
  /* =========================================================================
     Quick-add form — compact one-row layout above the buckets.
     ========================================================================= */
  .reminder-quick-add {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    padding: 0.75rem;
    border: 1px solid var(--color-border, oklch(0.9 0 0));
    border-radius: 0.5rem;
    background: var(--color-surface, #fff);
  }

  .reminder-quick-add__title {
    flex: 1;
    min-width: 0;
    border: none;
    outline: none;
    background: transparent;
    font-size: 0.875rem;
    color: var(--color-fg, oklch(0.2 0 0));
    padding: 0.25rem 0;
  }

  .reminder-quick-add__title::placeholder {
    color: var(--color-fg-muted, #5c4a37);
  }

  .reminder-quick-add__title:focus {
    outline: none;
  }

  .reminder-quick-add__due-label {
    font-size: 0.75rem;
    color: var(--color-fg-muted, #5c4a37);
    white-space: nowrap;
    flex-shrink: 0;
  }

  .reminder-quick-add__due {
    border: 1px solid var(--color-border, oklch(0.9 0 0));
    border-radius: 0.375rem;
    padding: 0.25rem 0.5rem;
    font-size: 0.8125rem;
    color: var(--color-fg, oklch(0.2 0 0));
    background: var(--color-surface, #fff);
    flex-shrink: 0;
  }

  .reminder-quick-add__due:focus {
    outline: 2px solid currentColor;
    outline-offset: 2px;
  }

  /* =========================================================================
     Page wrapper — single calm column, no second permanent pane.
     ========================================================================= */
  .reminders-page {
    display: flex;
    flex-direction: column;
    gap: 1.5rem;
  }

  /* Bucket section — heading + list. */
  .reminders-bucket {
    display: flex;
    flex-direction: column;
    gap: 0.375rem;
  }

  /* Bucket heading — understated label above the list. */
  .reminders-bucket__heading {
    display: flex;
    align-items: center;
    gap: 0.375rem;
    font-size: 0.6875rem;
    font-weight: 600;
    letter-spacing: 0.06em;
    text-transform: uppercase;
    color: var(--color-fg-muted, #5c4a37);
    padding-bottom: 0.25rem;
    border-bottom: 1px solid var(--color-border, oklch(0.9 0 0));
    margin-bottom: 0.125rem;
    pointer-events: none;
  }

  .reminders-bucket__heading--overdue {
    color: var(--color-fg-error, #a8331f);
  }

  .reminders-done-count {
    color: var(--color-fg-muted, #5c4a37);
    font-weight: 400;
  }

  /* Done toggle */
  .reminders-done-toggle {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    background: none;
    border: none;
    padding: 0;
    cursor: pointer;
    width: 100%;
    text-align: left;
  }

  .reminders-done-toggle .reminders-bucket__heading {
    pointer-events: auto;
    flex: 1;
    margin-bottom: 0;
    border-bottom: none;
  }

  .reminders-done-toggle:focus-visible {
    outline: 2px solid currentColor;
    outline-offset: 2px;
    border-radius: 0.25rem;
  }

  .reminders-done-chevron {
    color: var(--color-fg-muted, #5c4a37);
    flex-shrink: 0;
    transition: transform 150ms ease;
  }

  .reminders-done-chevron--open {
    transform: rotate(180deg);
  }

  /* Reminder list — vertical stack, no markers. */
  .reminders-list {
    display: flex;
    flex-direction: column;
    list-style: none;
    padding: 0;
    margin: 0;
  }

  /* =========================================================================
     Edit sheet form — stacked fields inside the SlideOver body.
     ========================================================================= */
  .edit-sheet-form {
    display: flex;
    flex-direction: column;
    gap: 1.25rem;
    padding: 1.25rem 1rem;
  }

  .edit-sheet-datetime {
    width: 100%;
    border: 1px solid var(--color-border, oklch(0.9 0 0));
    border-radius: 0.375rem;
    padding: 0.5rem 0.75rem;
    font-size: 0.875rem;
    color: var(--color-fg, oklch(0.2 0 0));
    background: var(--color-surface, #fff);
  }

  .edit-sheet-datetime:focus {
    outline: 2px solid currentColor;
    outline-offset: 2px;
  }

  .edit-sheet-select {
    width: 100%;
    border: 1px solid var(--color-border, oklch(0.9 0 0));
    border-radius: 0.375rem;
    padding: 0.5rem 0.75rem;
    font-size: 0.875rem;
    color: var(--color-fg, oklch(0.2 0 0));
    background: var(--color-surface, #fff);
  }

  .edit-sheet-select:focus {
    outline: 2px solid currentColor;
    outline-offset: 2px;
  }

  .edit-sheet-error {
    font-size: 0.875rem;
    color: var(--color-fg-error, #a8331f);
  }

  .edit-sheet-footer {
    display: flex;
    justify-content: flex-end;
    padding-top: 0.5rem;
  }
</style>
