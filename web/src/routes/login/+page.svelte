<script lang="ts">
  // Login page — minimal form shell. Auth logic is added by feature work.
  import { Button } from "@qovira/ui";
</script>

<div class="flex min-h-screen items-center justify-center">
  <form class="flex w-full max-w-sm flex-col gap-4">
    <h1 class="text-text text-xl font-semibold">Sign in</h1>
    <div class="flex flex-col gap-1">
      <label class="text-text text-sm" for="email">Email</label>
      <input
        class="border-border bg-surface text-text rounded border px-3 py-2 text-sm"
        id="email"
        name="email"
        type="email"
        autocomplete="email"
      />
    </div>
    <div class="flex flex-col gap-1">
      <label class="text-text text-sm" for="password">Password</label>
      <input
        class="border-border bg-surface text-text rounded border px-3 py-2 text-sm"
        id="password"
        name="password"
        type="password"
        autocomplete="current-password"
      />
    </div>
    <Button variant="primary" type="submit">Sign in</Button>
  </form>
</div>
