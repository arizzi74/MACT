#!/usr/bin/env node
'use strict';
// Check layout and capability targeting; desktop WebKit cannot render iOS's native status-bar blur.
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const {chromium,webkit}=require('playwright');
const assets=process.env.WEBUI_ASSETS || path.resolve(__dirname, '../internal/admin/static');
const selected={session_id:'a3c79015-ff31-4051-8e22-2998e49fc7f5',codex_thread_id:'thread',worker_id:'worker',worker_name:'Test worker',runtime_id:'runtime',runtime_name:'Primary Codex',name:'Header layout',cwd:'/project',state:'idle'};
(async()=>{
 const report=[];
 for(const [engine,type] of [['chromium',chromium],['webkit',webkit]]){
  const browser=await type.launch({headless:true});
  try{
   for(const fixture of [
    {name:'iPhone Home Screen',width:402,height:874,standalone:true,touch:5,inset:0,want:20},
    {name:'iPhone edge-to-edge',width:402,height:874,standalone:true,touch:5,inset:62,want:82},
    {name:'iPhone Safari tab',width:402,height:874,standalone:false,touch:5,inset:0,want:0},
    {name:'Android installed app',width:412,height:915,standalone:null,touch:5,inset:0,want:0},
    {name:'iPad desktop UA',width:1024,height:1366,standalone:true,touch:5,inset:24,want:44,userAgent:'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/27.2 Safari/605.1.15'},
    {name:'Mac installed app',width:1280,height:850,standalone:true,touch:0,inset:0,want:0},
   ]){
    const page=await browser.newPage({viewport:{width:fixture.width,height:fixture.height},hasTouch:fixture.touch>0,...(fixture.userAgent?{userAgent:fixture.userAgent}:{})});
    const errors=[];page.on('pageerror',e=>errors.push(e.message));
    await page.route('https://header.test/**',route=>{
     const u=new URL(route.request().url());const json=x=>route.fulfill({contentType:'application/json',body:JSON.stringify(x)});
     if(u.pathname.endsWith('/admin/session'))return json({authenticated:true,owner_id:'owner',session_id:'login',server_time:new Date().toISOString(),expires_at:new Date(Date.now()+28800000).toISOString()});
     if(u.pathname.endsWith('/login/options'))return json({passkey:true,password:false});
     if(u.pathname.endsWith('/sessions'))return json({sessions:[selected],workers:[],runtimes:[]});
     if(u.pathname.endsWith('/telegram-bots'))return json({bots:[],revision:'1'});
     if(u.pathname.endsWith('/question-answers'))return json({answers:[]});
     if(u.pathname.endsWith('/push/config'))return json({supported:false,subscribed:false});
     const name=u.pathname.includes('/static/')?path.basename(u.pathname):'webui.html';
     const file=path.join(assets,name);if(!fs.existsSync(file))return route.fulfill({status:404,body:''});
     let body=fs.readFileSync(file);if(name==='webui.css'&&fixture.inset)body=body.toString().replaceAll('env(safe-area-inset-top,0px)',fixture.inset+'px');
     return route.fulfill({contentType:name.endsWith('.js')?'text/javascript':name.endsWith('.css')?'text/css':'text/html',body});
    });
    await page.addInitScript(({fixture,selected})=>{
     Object.defineProperty(navigator,'standalone',{get:()=>fixture.standalone});
     Object.defineProperty(navigator,'maxTouchPoints',{get:()=>fixture.touch});
     window.WebSocket=class{
      static OPEN=1;
      constructor(url){this.readyState=0;this.activity=new URL(url).pathname.endsWith('/activity');setTimeout(()=>{this.readyState=1;this.emit(this.activity?{type:'activity_snapshot',version:3,sequence:0,revision:1,sessions:[{session_id:selected.session_id,state:'idle',pending_questions:0}],telegram_bots:[],telegram_bots_revision:'1'}:{type:'ready'});},1);}
      emit(x){this.onmessage?.({data:JSON.stringify(x)});}
      close(){this.readyState=3;this.onclose?.({code:1000});}
      send(raw){const f=JSON.parse(raw);let result={};if(f.method==='thread/resume')result={thread:{id:'thread'}};else if(f.method==='thread/items/list')result={data:[{turnId:'turn',item:{id:'message',type:'agentMessage',text:'A sharp header and a usable composer.'}}],nextCursor:null};else if(f.method==='thread/turns/list')result={data:[{id:'turn',status:'completed',items:[]}],nextCursor:null};else if(f.method==='gateway/questions')result={questions:[]};else if(f.method==='model/list')result={data:[]};setTimeout(()=>this.emit({id:f.id,result}),1);}
     };
    },{fixture,selected});
    await page.goto('https://header.test/tgw/webui/?session_id='+selected.session_id);
    await page.locator('#composer:not([hidden])').waitFor();
    await page.waitForTimeout(150);
    const measure=()=>page.evaluate(()=>{
     const shell=document.querySelector('#shell'),bar=document.querySelector('.topbar'),composer=document.querySelector('#composer');
     return {marked:document.documentElement.classList.contains('ios-home-screen'),padding:parseFloat(getComputedStyle(shell).paddingTop),headerTop:bar.getBoundingClientRect().top,shellTop:shell.getBoundingClientRect().top,composerBottom:composer.getBoundingClientRect().bottom,viewportHeight:visualViewport.height,bg:getComputedStyle(shell).backgroundColor,rootBg:getComputedStyle(document.documentElement).backgroundColor,scrollWidth:document.documentElement.scrollWidth,width:innerWidth};
    });
    const actual=await measure();assert.equal(actual.padding,fixture.want,fixture.name);assert.equal(actual.marked,fixture.want>0,fixture.name);
    assert.ok(Math.abs(actual.headerTop-actual.shellTop-fixture.want)<1,JSON.stringify(actual));
    assert.ok(actual.composerBottom<=actual.viewportHeight+1,fixture.name+' composer remains visible');
    assert.ok(actual.scrollWidth<=actual.width,fixture.name+' no horizontal overflow');
    if(fixture.want){assert.equal(actual.bg,actual.rootBg);await page.evaluate(()=>document.documentElement.setAttribute('data-theme','light'));const light=await measure();assert.equal(light.bg,light.rootBg);}
    if(fixture.width<=850&&fixture.want){await page.locator('#show-sessions').click();const brandTop=await page.locator('.brand').evaluate(x=>x.getBoundingClientRect().top);assert.ok(brandTop>=fixture.want,'Drawer brand clears blur');}
    if(fixture.want>0){
     if(fixture.width<=850)await page.locator('#close-sessions').click();
     await page.setViewportSize({width:fixture.width,height:450});
     await page.locator('#prompt').focus();await page.waitForTimeout(350);
     const resized=await measure();assert.ok(resized.composerBottom<=451,'Composer survives keyboard-sized viewport');assert.ok(resized.headerTop>=fixture.want-1,'Header clearance survives resize');
     await page.setViewportSize({width:fixture.width,height:fixture.height});await page.waitForTimeout(350);
    }
    assert.deepEqual(errors,[]);
    if(process.env.WEBUI_SCREENSHOTS && engine==='webkit'&&fixture.name==='iPhone Home Screen'){fs.mkdirSync(process.env.WEBUI_SCREENSHOTS,{recursive:true});await page.screenshot({path:path.join(process.env.WEBUI_SCREENSHOTS,'ios-header-layout.png')});}
    report.push({engine,name:fixture.name,...actual});await page.close();
   }
  }finally{await browser.close();}
 }
 console.log('Header clearance, drawer, viewport, composer, overflow and theme checks passed in Chromium and WebKit: '+report.length+' cases.');
})().catch(e=>{console.error(e);process.exit(1)});
