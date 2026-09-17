/* Asks AMP's own plugin loader to load ours.
 *
 * nginx injects a script tag pointing here into the panel's page. This file is
 * deliberately tiny and deliberately defensive: it is running inside somebody
 * else's application, and the worst thing it could do is throw an exception
 * that looks like AMP's fault.
 *
 * The hook is PluginHandler.SetFeatures, which AMP calls once, after the
 * session exists and immediately before it loads the plugins its backend
 * listed. Waiting for that moment rather than polling means the tab appears
 * with the others rather than after them.
 */
(function () {
    'use strict';

    const NAME = 'AmpBB';

    function load() {
        try {
            if (typeof PluginHandler === 'undefined' || !PluginHandler.LoadPluginAsync) { return; }
            if (PluginHandler.PluginIsLoaded && PluginHandler.PluginIsLoaded(NAME)) { return; }
            Promise.resolve(PluginHandler.LoadPluginAsync(NAME)).catch(function (err) {
                console.error('amp-bb: the tab could not be loaded:', err);
            });
        } catch (err) {
            console.error('amp-bb: the tab could not be loaded:', err);
        }
    }

    function hook() {
        if (typeof PluginHandler === 'undefined' || !PluginHandler.SetFeatures) { return false; }
        const original = PluginHandler.SetFeatures;
        PluginHandler.SetFeatures = function () {
            const result = original.apply(this, arguments);
            /* Out of line, so that a failure here cannot break AMP's own
             * start-up sequence. */
            setTimeout(load, 0);
            return result;
        };
        return true;
    }

    if (hook()) { return; }

    /* PluginHandler.js is loaded with defer, as is this file, so it should
     * already be there. If the panel ever reorders its scripts, fall back to
     * waiting a few seconds rather than giving up silently. */
    let waited = 0;
    const timer = setInterval(function () {
        waited += 100;
        if (hook() || waited > 15000) { clearInterval(timer); }
    }, 100);
})();
