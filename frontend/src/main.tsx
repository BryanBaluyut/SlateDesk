import "./index.css";

import {
  QueryCache,
  QueryClient,
  QueryClientProvider,
} from "@tanstack/react-query";
import { createRouter, RouterProvider } from "@tanstack/react-router";
import { StrictMode } from "react";
import ReactDOM from "react-dom/client";

import { isApiError } from "@/api/client";
import {
  RouteErrorFallback,
  RouteNotFound,
  RoutePending,
} from "@/components/route-errors";
import { ThemeProvider } from "@/components/theme";
import { routeTree } from "./routeTree.gen";

const queryClient = new QueryClient({
  queryCache: new QueryCache({
    onError: (error) => {
      // Session died mid-app (expired cookie, token_version bump): go back
      // to login. The login page runs no authenticated queries, so this
      // cannot loop.
      if (
        isApiError(error) &&
        error.status === 401 &&
        window.location.pathname !== "/login"
      ) {
        const redirect = encodeURIComponent(
          window.location.pathname + window.location.search,
        );
        window.location.assign(`/login?redirect=${redirect}`);
      }
    },
  }),
  defaultOptions: {
    queries: {
      staleTime: 30_000,
      retry: (failureCount, error) => {
        // Never retry client errors; they will not get better.
        if (isApiError(error) && error.status < 500) {
          return false;
        }
        return failureCount < 2;
      },
    },
  },
});

const router = createRouter({
  routeTree,
  context: { queryClient },
  defaultPreload: "intent",
  defaultPreloadStaleTime: 0,
  defaultErrorComponent: RouteErrorFallback,
  defaultNotFoundComponent: RouteNotFound,
  // Entry navigations (cold load, direct ticket links) block on loaders;
  // without a pending component the user stares at a blank viewport.
  defaultPendingComponent: RoutePending,
});

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}

const rootElement = document.getElementById("root");
if (!rootElement) {
  throw new Error("missing #root element");
}

ReactDOM.createRoot(rootElement).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <ThemeProvider>
        <RouterProvider router={router} />
      </ThemeProvider>
    </QueryClientProvider>
  </StrictMode>,
);
