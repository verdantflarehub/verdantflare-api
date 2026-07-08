import { registerBrowserEnv } from '@visactor/vchart/esm/env';

let vchartEnvironmentInitialized = false;

export function initializeVChartEnvironment() {
  if (vchartEnvironmentInitialized || typeof window === 'undefined') {
    return;
  }

  registerBrowserEnv();
  vchartEnvironmentInitialized = true;
}
