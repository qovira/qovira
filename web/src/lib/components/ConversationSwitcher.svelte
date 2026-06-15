<script lang="ts">
  // ConversationSwitcher — slide-over panel listing the user's conversations.
  //
  // Behaviour:
  //   - On open: fetches the first page of conversations (most-recently-active first)
  //     via GET /api/v1/conversations.
  //   - "Load more" button pages through the cursor-paginated list.
  //   - Selecting a conversation calls switchConversation(id) (which resets
  //     tool-calls + confirmations, fetches history, sets the active conversation),
  //     then closes the panel.
  //   - "New conversation" calls startNewConversation() (resets stores, mints a
  //     fresh ULID, empty history) and closes the panel.
  //
  // Props:
  //   open         — bindable; controls the slide-over visibility.
  //   onconvclose  — callback prop called when the panel closes (via selection,
  //                  new conversation, Esc, or overlay click).
  //   focusComposer — callback prop called when "New conversation" is selected,
  //                   so the parent can focus the composer textarea.

  import type { components } from "$lib/api/schema.d.ts";
  import { Api } from "$lib/api/index.js";
  import { getActiveConversationId } from "$lib/stores/conversation.svelte.js";
  import { switchConversation, startNewConversation } from "$lib/stores/switch-conversation.js";
  import SlideOver from "$lib/components/SlideOver.svelte";
  import {
    conv_switcher_title,
    conv_switcher_new,
    conv_switcher_load_more,
    conv_switcher_empty,
    conv_switcher_new_preview,
    conv_switcher_load_error,
    conv_switcher_retry,
  } from "$lib/paraglide/messages.js";

  type Conversation = components["schemas"]["Conversation"];

  interface Props {
    open?: boolean;
    onconvclose?: () => void;
    focusComposer?: () => void;
  }

  let { open = $bindable(false), onconvclose, focusComposer }: Props = $props();

  // ---------------------------------------------------------------------------
  // Conversation list state
  // ---------------------------------------------------------------------------
  let conversations = $state<Conversation[]>([]);
  let nextCursor = $state<string | null>(null);
  let hasMore = $state(false);
  let loading = $state(false);
  let loadingMore = $state(false);
  /**
   * True when loadPage() threw (network error, unexpected server error).
   * Distinct from an empty list so the UI can show "couldn't load / retry"
   * instead of the "No conversations yet" empty-state, which would be a lie.
   */
  let loadError = $state(false);

  // Current active conversation — derived reactively from the store.
  const activeId = $derived(getActiveConversationId());

  // ---------------------------------------------------------------------------
  // Load the first page when the panel opens.
  // $effect is appropriate: syncing with an external I/O system (the API)
  // triggered by a DOM/prop state change.
  // ---------------------------------------------------------------------------
  $effect(() => {
    if (!open) return;
    // Reset and reload the list each time the panel opens so it reflects the
    // latest state (e.g. a new conversation that materialized since last open).
    conversations = [];
    nextCursor = null;
    hasMore = false;
    void loadPage(undefined);
  });

  // ---------------------------------------------------------------------------
  // Pagination
  // ---------------------------------------------------------------------------

  async function loadPage(cursor: string | undefined): Promise<void> {
    if (cursor === undefined) {
      loading = true;
    } else {
      loadingMore = true;
    }
    loadError = false;

    try {
      const { data } = await Api.GET("/conversations", {
        params: { query: cursor !== undefined ? { cursor } : {} },
      });

      if (data !== undefined) {
        conversations = cursor === undefined ? data.data : [...conversations, ...data.data];
        nextCursor = data.pagination.nextCursor;
        hasMore = data.pagination.hasMore;
      }
    } catch {
      // Network throw or unexpected error — flag it so the UI can distinguish
      // a transient error from a genuine empty conversation list.
      loadError = true;
    } finally {
      loading = false;
      loadingMore = false;
    }
  }

  async function handleLoadMore(): Promise<void> {
    if (nextCursor === null || loadingMore) return;
    await loadPage(nextCursor);
  }

  // ---------------------------------------------------------------------------
  // Selection handlers
  // ---------------------------------------------------------------------------

  async function handleSelect(id: string): Promise<void> {
    // switchConversation resets tool-calls + confirmations, fetches history,
    // and calls setActiveConversation.
    await switchConversation(id);
    close();
  }

  function handleNewConversation(): void {
    startNewConversation();
    close();
    // Focus the composer so the user can start typing immediately.
    focusComposer?.();
  }

  function close(): void {
    open = false;
    onconvclose?.();
  }

  // ---------------------------------------------------------------------------
  // Preview label helper — calm fallback for empty/new conversations
  // ---------------------------------------------------------------------------

  function previewLabel(conv: Conversation): string {
    return conv.preview.trim() !== "" ? conv.preview : conv_switcher_new_preview();
  }
</script>

<SlideOver bind:open title={conv_switcher_title()} onclose={close}>
  <!-- "New conversation" action at the top -->
  <div class="border-b border-border px-4 py-3">
    <button
      type="button"
      class="bg-primary text-on-primary w-full rounded-lg px-4 py-2 text-sm font-medium
             hover:opacity-90 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-current
             disabled:cursor-not-allowed disabled:opacity-50"
      onclick={handleNewConversation}
    >
      {conv_switcher_new()}
    </button>
  </div>

  <!-- Conversation list -->
  <div class="flex flex-col">
    {#if loading}
      <!-- Loading state -->
      <ul class="flex flex-col" role="list">
        {#each [0, 1, 2, 3, 4] as skeletonIdx (skeletonIdx)}
          <li class="px-4 py-3">
            <div class="bg-surface-raised h-4 animate-pulse rounded"></div>
          </li>
        {/each}
      </ul>
    {:else if loadError}
      <!-- Error state — network throw; distinguish from a genuine empty list -->
      <div class="flex flex-col items-start gap-2 px-4 py-6">
        <p class="text-text-subtle text-sm">{conv_switcher_load_error()}</p>
        <button
          type="button"
          class="text-text-subtle hover:text-text text-sm underline focus-visible:outline-2
                 focus-visible:outline-offset-2 focus-visible:outline-current"
          onclick={() => void loadPage(undefined)}
        >
          {conv_switcher_retry()}
        </button>
      </div>
    {:else if conversations.length === 0}
      <p class="text-text-subtle px-4 py-6 text-sm">{conv_switcher_empty()}</p>
    {:else}
      <ul class="flex flex-col" role="list">
        {#each conversations as conv (conv.id)}
          <li>
            <button
              type="button"
              class="w-full px-4 py-3 text-left text-sm transition-colors
                     hover:bg-surface-raised focus-visible:outline-2 focus-visible:outline-offset-[-2px]
                     focus-visible:outline-current
                     {activeId === conv.id ? 'bg-surface-raised font-medium text-text' : 'text-text-subtle'}"
              onclick={() => void handleSelect(conv.id)}
              aria-current={activeId === conv.id ? "true" : undefined}
            >
              <span class="block truncate">{previewLabel(conv)}</span>
            </button>
          </li>
        {/each}
      </ul>

      <!-- Pagination: "Load more" cursor button -->
      {#if hasMore}
        <div class="px-4 py-3">
          <button
            type="button"
            disabled={loadingMore}
            class="text-text-subtle w-full rounded px-2 py-1.5 text-sm
                   hover:bg-surface-raised hover:text-text
                   focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-current
                   disabled:cursor-not-allowed disabled:opacity-50"
            onclick={() => void handleLoadMore()}
          >
            {conv_switcher_load_more()}
          </button>
        </div>
      {/if}
    {/if}
  </div>
</SlideOver>
