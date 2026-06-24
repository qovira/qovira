<script lang="ts">
  // SlideOver fixture — a minimal wrapper that manages the open state via $state
  // so Storybook story snippets can bind to it. Used only by SlideOver.stories.svelte.

  import SlideOver from "./SlideOver.svelte";

  interface Props {
    onclose?: () => void;
    showProgrammaticClose?: boolean;
  }

  const { onclose, showProgrammaticClose = false }: Props = $props();

  let open = $state(false);
</script>

<button
  type="button"
  aria-label="Open slide-over"
  onclick={() => {
    open = true;
  }}
>
  Open slide-over
</button>

{#if showProgrammaticClose}
  <button
    type="button"
    aria-label="Close programmatically"
    onclick={() => {
      open = false;
    }}
  >
    Close programmatically
  </button>
{/if}

<SlideOver
  bind:open
  title="Conversations"
  onclose={() => {
    open = false;
    onclose?.();
  }}
>
  {#snippet children()}
    <p class="p-4 text-sm">Slide-over content.</p>
  {/snippet}
</SlideOver>
