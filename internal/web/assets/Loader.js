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

    /* nginx puts this script into every page the panel serves: the
     * controller's instance list as well as each instance's own view. The tab
     * belongs to exactly one of them -- the instance this daemon backs up.
     *
     * Anywhere else it is wrong in one of two ways. On the controller the
     * session is a controller session, which an instance does not accept, so
     * the tab appears and then reports that it was rejected. On another
     * instance it would be worse than useless: it would show one server's
     * snapshots to somebody who is looking at another server, with a restore
     * button under them.
     *
     * The daemon substitutes its own instance id here when it serves this
     * file. An unsubstituted placeholder means it was not configured with one,
     * and then any instance view will do -- but never the controller. */
    const INSTANCE = '@AMPBB_INSTANCE_ID@';

    /* Mirrors AMP's own checkADSLogin (AMP.js): an instance view is
     * /remote/<id>/..., /instance/<id>/..., or ?remote= / ?instance=. */
    function instanceInView() {
        const parts = location.pathname.split('/').filter(Boolean);
        if ((parts[0] === 'remote' || parts[0] === 'instance') && parts.length > 1) {
            return parts[1];
        }
        try {
            const query = new URLSearchParams(location.search);
            return query.get('remote') || query.get('instance') || '';
        } catch (err) {
            return '';
        }
    }

    function belongsOnThisPage() {
        const here = instanceInView();
        if (!here) { return false; }
        if (!INSTANCE || INSTANCE.charAt(0) === '@') { return true; }
        return here.toLowerCase() === INSTANCE.toLowerCase();
    }

    if (!belongsOnThisPage()) { return; }

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
