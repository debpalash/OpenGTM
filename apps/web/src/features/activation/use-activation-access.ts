import { useQuery } from "@tanstack/react-query"
import { getActiveWorkspace } from "@/lib/auth"
import { fetchActivationAccess } from "./destination-client"

export function useActivationAccess() {
  return useQuery({
    queryKey: ["activation-access", getActiveWorkspace()],
    queryFn: ({ signal }) => fetchActivationAccess(signal),
    staleTime: 15000,
    refetchInterval: 30000
  })
}
