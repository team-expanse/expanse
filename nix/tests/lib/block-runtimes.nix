# Registers block types' runtimes in a test node's store, as a binary cache would serve them, so the
# agent's real path (link the runtime, put it on the unit's PATH) runs with no network.
types:
{ config, ... }:
{
  virtualisation.additionalPaths = map (t: config.expanse.agent.blockRuntimes.${t}) types;
}
