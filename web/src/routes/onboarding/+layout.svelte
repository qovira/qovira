<script lang="ts">
  import type { Snippet } from "svelte";

  // Onboarding route host — renders children as-is. Full onboarding flow is owned by the Admin Onboarding feature;
  // only the host layout lives here.
  interface Props {
    children: Snippet;
  }

  const { children }: Props = $props();
</script>

{@render children()}
