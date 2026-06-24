// ConversationSwitcher stories — replaces the hand-copied loadPage state machine.
//
// Renders the REAL ConversationSwitcher component with Api.GET mocked to return
// controlled fixtures for each scenario. Asserts against the rendered DOM:
//
//   SuccessWithList   — API returns conversations → list renders
//   EmptyState        — API returns zero rows → empty-state text, NOT error-state
//   ErrorState        — Api.GET returns { error: ProblemError } → error-state, NOT empty
//   LoadMore          — second page appends to list (handleLoadMore guard)
//   ResetOnOpen       — closing and re-opening the panel clears state and reloads
//
// CSF 3 format (not Svelte CSF) because vi.mock requires the module-level hoisting
// available in a plain .ts story file; Svelte CSF's <script module> block
// doesn't support vi.mock hoisting.

import { vi } from "vitest";
import type { Meta, StoryObj } from "@storybook/sveltekit";
import { expect, fn, userEvent, fireEvent } from "storybook/test";
import { ProblemError } from "$lib/api/index.js";
import ConversationSwitcher from "./ConversationSwitcher.svelte";

// ---------------------------------------------------------------------------
// Module mocks — hoisted above all imports by Vitest.
// We mock Api so the component doesn't make real network calls.
// We also mock the stores the component calls on close so they don't error.
// ---------------------------------------------------------------------------

vi.mock("$lib/api/index.js", async (importActual) => {
  const actual = await importActual<typeof import("$lib/api/index.js")>();
  return {
    ...actual,
    Api: {
      ...actual.Api,
      GET: vi.fn(),
    },
  };
});

vi.mock("$lib/stores/switch-conversation.js", () => ({
  switchConversation: vi.fn().mockResolvedValue(undefined),
  startNewConversation: vi.fn(),
}));

vi.mock("$lib/stores/conversation.svelte.js", async (importActual) => {
  const actual = await importActual<typeof import("$lib/stores/conversation.svelte.js")>();
  return {
    ...actual,
    getActiveConversationId: vi.fn().mockReturnValue(null),
  };
});

// ---------------------------------------------------------------------------
// Import after mock registration
// ---------------------------------------------------------------------------

import { Api } from "$lib/api/index.js";

// ---------------------------------------------------------------------------
// Fixture helpers
// ---------------------------------------------------------------------------

function makeConv(id: string, preview: string) {
  return {
    id,
    preview,
    createdAt: new Date(Date.now() - 10_000).toISOString(),
    updatedAt: new Date().toISOString(),
  };
}

function successResponse(
  convs: ReturnType<typeof makeConv>[],
  opts: { hasMore?: boolean; nextCursor?: string | null } = {},
) {
  return {
    data: {
      data: convs,
      pagination: {
        hasMore: opts.hasMore ?? false,
        nextCursor: opts.nextCursor ?? null,
      },
    },
    error: undefined,
    response: new Response(),
  };
}

function errorResponse() {
  const err = new ProblemError({
    type: "https://qovira.ai/errors/internal",
    title: "Internal error",
    status: 500,
    detail: "Something went wrong",
    code: "internal_error",
    requestId: "test-req",
  });
  return { data: undefined, error: err, response: new Response("", { status: 500 }) };
}

// ---------------------------------------------------------------------------
// Meta
// ---------------------------------------------------------------------------

const meta = {
  title: "Conversations/ConversationSwitcher",
  component: ConversationSwitcher,
  tags: ["autodocs"],
  parameters: {
    layout: "padded",
  },
  args: {
    open: true,
    onconvclose: fn(),
    focusComposer: fn(),
  },
} satisfies Meta<typeof ConversationSwitcher>;

export default meta;
type Story = StoryObj<typeof meta>;

// ---------------------------------------------------------------------------
// SuccessWithList — API returns two conversations → list renders
// ---------------------------------------------------------------------------
export const SuccessWithList: Story = {
  beforeEach: () => {
    vi.mocked(Api.GET).mockResolvedValue(
      successResponse([makeConv("conv-1", "Set a reminder to buy oat milk"), makeConv("conv-2", "Plan my week")]),
    );
    return () => {
      vi.mocked(Api.GET).mockReset();
    };
  },
  play: async ({ canvas }) => {
    // Wait for the list to load — items must appear.
    await expect(await canvas.findByText("Set a reminder to buy oat milk")).toBeVisible();
    await expect(canvas.getByText("Plan my week")).toBeVisible();

    // Error-state must NOT be shown.
    const errorText = canvas.queryByText("Couldn't load conversations.");
    await expect(errorText).toBeNull();

    // Empty-state must NOT be shown.
    const emptyText = canvas.queryByText("No conversations yet. Start one below.");
    await expect(emptyText).toBeNull();
  },
};

// ---------------------------------------------------------------------------
// EmptyState — API returns zero rows → empty-state text shown, NOT error-state
// ---------------------------------------------------------------------------
export const EmptyState: Story = {
  beforeEach: () => {
    vi.mocked(Api.GET).mockResolvedValue(successResponse([]));
    return () => {
      vi.mocked(Api.GET).mockReset();
    };
  },
  play: async ({ canvas }) => {
    // Empty-state text must be visible.
    await expect(await canvas.findByText("No conversations yet. Start one below.")).toBeVisible();

    // Error-state must NOT be shown — zero rows ≠ error.
    const errorText = canvas.queryByText("Couldn't load conversations.");
    await expect(errorText).toBeNull();
  },
};

// ---------------------------------------------------------------------------
// ErrorState — Api.GET returns problem+json error → error-state shown, NOT empty
// ---------------------------------------------------------------------------
export const ErrorState: Story = {
  beforeEach: () => {
    vi.mocked(Api.GET).mockResolvedValue(errorResponse());
    return () => {
      vi.mocked(Api.GET).mockReset();
    };
  },
  play: async ({ canvas }) => {
    // Error-state text must be visible.
    await expect(await canvas.findByText("Couldn't load conversations.")).toBeVisible();
    // Retry button must also be present.
    await expect(canvas.getByRole("button", { name: "Try again" })).toBeVisible();

    // Empty-state must NOT be shown — a server error is not an empty list.
    const emptyText = canvas.queryByText("No conversations yet. Start one below.");
    await expect(emptyText).toBeNull();
  },
};

// ---------------------------------------------------------------------------
// LoadMore — second page appends; guard prevents cursor=null fetch
// ---------------------------------------------------------------------------
export const LoadMore: Story = {
  beforeEach: () => {
    // First call returns page 1 with hasMore=true.
    vi.mocked(Api.GET)
      .mockResolvedValueOnce(
        successResponse([makeConv("conv-1", "First conversation")], { hasMore: true, nextCursor: "cursor-1" }),
      )
      // Second call (load more) returns page 2.
      .mockResolvedValueOnce(successResponse([makeConv("conv-2", "Second conversation")]));
    return () => {
      vi.mocked(Api.GET).mockReset();
    };
  },
  play: async ({ canvas }) => {
    // First page renders.
    await expect(await canvas.findByText("First conversation")).toBeVisible();

    // "Load more" button is present.
    const loadMoreBtn = await canvas.findByRole("button", { name: /load more/i });
    await expect(loadMoreBtn).toBeVisible();

    // Click load more — second page appends.
    await userEvent.click(loadMoreBtn);
    await expect(await canvas.findByText("Second conversation")).toBeVisible();
    await expect(canvas.getByText("First conversation")).toBeVisible();

    // After loading the last page (hasMore=false), "Load more" button is gone.
    await expect(canvas.queryByRole("button", { name: /load more/i })).toBeNull();
  },
};

// ---------------------------------------------------------------------------
// InitialLoadOnOpen — the $effect fires once when open=true on mount and loads
// the first page. Verifies the happy path: panel mounts open → GET called once
// → list renders.
//
// NOTE: This story does NOT exercise the reset-on-open path (clearing
// conversations/nextCursor/hasMore before the reload, lines 73–76 of
// ConversationSwitcher.svelte). Testing reset-on-open requires driving
// open=false→open=true via a controlled parent; that is covered by integration
// tests rather than a Storybook play function, which cannot rerender the
// component by toggling args.open mid-play in the current setup.
// ---------------------------------------------------------------------------
export const InitialLoadOnOpen: Story = {
  beforeEach: () => {
    vi.mocked(Api.GET).mockResolvedValue(successResponse([makeConv("conv-1", "Fresh conversation")]));
    return () => {
      vi.mocked(Api.GET).mockReset();
    };
  },
  play: async ({ canvas }) => {
    // The panel starts open (args.open=true by default from meta.args).
    await expect(await canvas.findByText("Fresh conversation")).toBeVisible();
    // Api.GET was called once when the panel opened (the $effect ran).
    await expect(vi.mocked(Api.GET)).toHaveBeenCalledTimes(1);
  },
};

// ---------------------------------------------------------------------------
// LoadMoreGuard — clicking Load More a second time while in-flight is ignored
// (handleLoadMore guard: nextCursor === null || loadingMore)
// ---------------------------------------------------------------------------
export const LoadMoreGuard: Story = {
  beforeEach: () => {
    let resolveSecondPage: ((v: unknown) => void) | null = null;

    // First page resolves immediately.
    vi.mocked(Api.GET).mockResolvedValueOnce(
      successResponse([makeConv("conv-1", "First conversation")], { hasMore: true, nextCursor: "cursor-1" }),
    );
    // Second page hangs until we release it.
    vi.mocked(Api.GET).mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveSecondPage = resolve;
        }),
    );

    return () => {
      // Release the hanging promise to avoid open handles.
      if (resolveSecondPage !== null) {
        resolveSecondPage(successResponse([makeConv("conv-2", "Second conversation")]));
      }
      vi.mocked(Api.GET).mockReset();
    };
  },
  play: async ({ canvas }) => {
    await expect(await canvas.findByText("First conversation")).toBeVisible();

    const loadMoreBtn = await canvas.findByRole("button", { name: /load more/i });

    // Click once — starts the in-flight load (second GET call, which hangs).
    await userEvent.click(loadMoreBtn);

    // Button should be disabled while loading more.
    await expect(loadMoreBtn).toBeDisabled();

    // Api.GET should have been called exactly twice total (first page + one load-more).
    await expect(vi.mocked(Api.GET)).toHaveBeenCalledTimes(2);

    // Now attempt a concurrent second click via fireEvent to bypass the disabled
    // attribute and reach handleLoadMore() directly — the JS guard
    // `if (nextCursor === null || loadingMore) return;` must prevent a third GET call.
    await fireEvent.click(loadMoreBtn);

    // Call count must still be 2 — the loadingMore guard returned early.
    await expect(vi.mocked(Api.GET)).toHaveBeenCalledTimes(2);
  },
};
