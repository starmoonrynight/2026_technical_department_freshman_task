// Render the editable HTML wireframe and capture real pages from a running local server.
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright-core');
const path = require('node:path');
const {pathToFileURL}=require('node:url');
const root=path.resolve(__dirname,'..');
const base=process.env.PREVIEW_URL||'http://127.0.0.1:4173';
(async()=>{
  const browser=await chromium.launch({headless:true,...(process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{})});
  try {
    const design=await browser.newPage({viewport:{width:1650,height:1000},deviceScaleFactor:2});
    await design.goto(pathToFileURL(path.join(root,'docs/page-design.html')).href);
    await design.screenshot({path:path.join(root,'docs/images/page-design.png'),fullPage:true});
    const desktop=await browser.newPage({viewport:{width:1440,height:1000}});
    await desktop.goto(base); await desktop.waitForSelector('.item-card');
    await desktop.screenshot({path:path.join(root,'docs/images/desktop-preview.png'),fullPage:true});
    const mobile=await browser.newPage({viewport:{width:390,height:844},isMobile:true,hasTouch:true});
    await mobile.goto(base); await mobile.waitForSelector('.item-card');
    await mobile.screenshot({path:path.join(root,'docs/images/mobile-preview.png')});
    await mobile.goto(base+'/#/login');
    await mobile.getByRole('button',{name:'填入小杭账号'}).click();
    await mobile.locator('#auth-form').getByRole('button',{name:'登录',exact:true}).last().click();
    await mobile.waitForSelector('.item-card');
    await mobile.goto(base+'/#/messages/c-demo'); await mobile.waitForSelector('.bubble');
    await mobile.locator('#toast').evaluate(el=>el.classList.remove('show'));
    await mobile.screenshot({path:path.join(root,'docs/images/chat-preview.png')});
    console.log('Rendered design and three page previews.');
  } finally {await browser.close();}
})().catch(e=>{console.error(e);process.exitCode=1;});
