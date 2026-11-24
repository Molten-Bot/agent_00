// Scheduling lives in Workflow; the Library shortcut carries task options into a draft.
const {before,after,test}=require('node:test');
const assert=require('node:assert/strict');
const {chromium}=require('playwright');
const baseURL=process.env.HARNESS_TEST_URL||'http://127.0.0.1:7777';
let browser;
before(async()=>{browser=await chromium.launch({headless:true,args:['--no-sandbox']})});
after(async()=>{await browser?.close()});
test('Library scheduling creates a workflow draft with its prompt and repository options',async t=>{
 const page=await browser.newPage({viewport:{width:1280,height:900}});const errors=[];page.on('pageerror',e=>errors.push(e.message));t.after(async()=>{await page.close();assert.deepEqual(errors,[])});
 const task={name:'unit-test-coverage',displayName:'Unit Test Coverage',type:'testing',prompt:'Improve unit tests.',description:'Add useful coverage'};
 let doc={version:2,revision:0,workflows:[]};let legacyCalls=0;
 await page.addInitScript(()=>{window.EventSource=class{}});
 await page.route('**/*',route=>{const url=new URL(route.request().url());if(url.origin!==new URL(baseURL).origin)return route.abort();
 if(url.pathname==='/api/workflows'){if(route.request().method()==='PUT')doc={...route.request().postDataJSON(),revision:doc.revision+1};return route.fulfill({json:doc})}
 if(url.pathname==='/api/workflows/schedules')return route.fulfill({json:[]});
 if(url.pathname==='/api/library')return route.fulfill({json:{ok:true,tasks:[task]}});
 if(url.pathname==='/api/library/schedules'){legacyCalls++;return route.fulfill({status:410,json:{error:'Scheduling moved to Workflow'}})}
 if(url.pathname==='/api/github/repos')return route.fulfill({json:{ok:true,repos:[{full_name:'acme/repo',html_url:'https://github.com/acme/repo'}]}});
 if(url.pathname==='/api/agent-auth')return route.fulfill({json:{ok:true,auth:{required:false,ready:true,state:'ready'}}});
 if(url.pathname==='/api/review-settings')return route.fulfill({json:{ok:true,settings:{workflow_enabled:true,default_library_enabled:true}}});
 if(url.pathname==='/api/status')return route.fulfill({json:{tasks:[],events:[],repositories:[],stats:{},resources:{}}});
 if(url.pathname.startsWith('/api/'))return route.fulfill({json:{ok:true}});return route.continue()});
 await page.goto(baseURL+'/#studio-library');
 await page.waitForFunction(()=>state.libraryTasks.length===1);
  assert.equal(await page.locator('#library-repo-input').inputValue(),'','repository stays empty until explicitly selected');
 await page.evaluate(()=>{libraryRepoInput.value='https://github.com/acme/repo.git';libraryBaseBranch.value='feature';libraryTargetSubdir.value='src';updateLibraryScheduleTrigger(state.libraryTasks[0]);});
 await page.locator('#library-schedule-trigger').click();
 await page.getByLabel('Trigger type').waitFor();assert.equal(new URL(page.url()).hash,'#workflow');assert.equal(await page.getByLabel('Trigger type').inputValue(),'schedule');assert.equal(await page.getByRole('button',{name:'acme/repo',exact:true}).getAttribute('aria-pressed'),'true');
 assert.equal(await page.locator('#library-schedule-modal').count(),0);assert.equal(await page.locator('#library-schedules-open').count(),0);
 await page.getByRole('button',{name:'weeks',exact:true}).click();assert.equal(await page.getByLabel('Repeat every (weeks)').inputValue(),'1');await page.getByRole('button',{name:'hours',exact:true}).click();await page.getByLabel('Repeat every (hours)').fill('10');
 await page.locator('[data-id="task"]').click();assert.equal(await page.getByLabel('Prompt source',{exact:true}).inputValue(),'unit-test-coverage');assert.equal(await page.getByLabel('Base branch (optional)').inputValue(),'feature');assert.equal(await page.getByLabel('Target directory (optional)').inputValue(),'src');
 await page.getByLabel('Additional context (optional)').fill('Focus on API coverage');await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');
 assert.equal(doc.workflows.length,1);assert.equal(doc.workflows[0].enabled,false);assert.equal(doc.workflows[0].repository,'acme/repo');assert.equal(doc.workflows[0].task.baseBranch,'feature');assert.equal(doc.workflows[0].task.targetSubdir,'src');assert.equal(doc.workflows[0].trigger.intervalHours,10);assert.equal(doc.workflows[0].task.libraryTaskName,'unit-test-coverage');assert.equal(doc.workflows[0].task.instructions,'Focus on API coverage');assert.equal(legacyCalls,0);
 await page.reload();await page.locator('.wf-step').first().click();assert.equal(await page.getByLabel('Repeat every (hours)').inputValue(),'10');
});
