/* SPDX-License-Identifier: Apache-2.0 */
/*
 * An NFS-Ganesha FSAL whose namespace is Forebay's.
 *
 * The spike in mem_forebay.c borrowed the memory FSAL's namespace and served
 * Forebay's bytes through it. This is the other half: an export whose files
 * are objects the node agent can answer for, so a path a client walks is a
 * path this decides, not one the memory FSAL invented.
 *
 * It is read-only. RFC-0021 makes a published dataset version immutable to
 * every path Forebay controls, and an FSAL that accepted a write would be the
 * one place that was not true.
 *
 * WHAT IS NOT HERE, and deliberately:
 *
 *   pNFS. RFC-0008 found that Ganesha's flexfiles helpers are absent from its
 *   export list, so an FSAL calling them does not link. Until a build exports
 *   them this is a plain NFS server reading through the agent, which is the
 *   configuration a stock client mounts today.
 *
 * This file needs NFS-Ganesha's headers and cannot be built in this
 * repository. Everything it decides that does not need them lives in
 * forebay_path.c and forebay_source.c, which are built and checked here.
 */

#include "config.h"

#include "fsal.h"
#include "fsal_types.h"
#include "FSAL/fsal_commonlib.h"
#include "fsal_api.h"
#include "config_parsing.h"
#include "nfs_exports.h"
#include "FSAL/fsal_init.h"

#include "forebay_client.h"
#include "forebay_path.h"
#include "forebay_source.h"

#include <stdlib.h>
#include <string.h>

#define FOREBAY_NAME "FOREBAY"

/* How much of a directory one answer carries. Names rather than bytes is what
 * the far side is told, and the buffer is sized for the worst case of that
 * many at full length.
 */
#define FOREBAY_LIST_NAMES 512
#define FOREBAY_LIST_BYTES (FOREBAY_LIST_NAMES * (FOREBAY_ENTRY_HEADER + FOREBAY_ENTRY_NAME_MAX + 1))

/* One connection per export rather than per process.
 *
 * Two exports are two tenants, and a tenant is on every request, so sharing a
 * connection would not be wrong. It would serialise them, which is the reason
 * not to: one export's slow miss to the backend would hold up the other's hit.
 */
struct forebay_export {
	struct fsal_export export;
	struct forebay_source *source;
	/* tenant is on every request. RFC-0016 gives each tenant its own export
	 * and lets the network path establish which one is calling, so it is
	 * configuration here rather than anything a client sends.
	 */
	char *tenant;
	struct forebay_handle *root;
};

/* A handle is a path in this export's namespace.
 *
 * The key is kept beside it rather than recomputed, because it is needed on
 * every read and computing it twice is two chances to compute it differently.
 */
struct forebay_handle {
	struct fsal_obj_handle obj_handle;
	struct forebay_export *export;
	char *name;
	char *key;
	/* size is what the agent last said. Held because getattrs is asked far
	 * more often than a file changes, and a published version does not
	 * change at all.
	 */
	uint64_t size;
	bool is_dir;
};

static struct fsal_staticfsinfo_t forebay_info = {
	.maxfilesize = UINT64_MAX,
	.maxlink = 0,
	.maxnamelen = FOREBAY_KEY_MAX,
	.maxpathlen = FOREBAY_KEY_MAX,
	.no_trunc = true,
	.chown_restricted = true,
	.case_preserving = true,
	.link_support = false,
	.symlink_support = false,
	.lock_support = false,
	.named_attr = false,
	.unique_handles = true,
	.acl_support = 0,
	.homogenous = true,
	.supported_attrs = ATTRS_POSIX,
	.link_supports_permission_checks = false,
};

/* The module carries the object ops vector, which is where every FSAL in the
 * tree keeps it: fsal_module itself has no room for one.
 */
struct forebay_fsal_module {
	struct fsal_module fsal;
	struct fsal_obj_ops handle_ops;
};

static struct forebay_fsal_module forebay_module;

/* status_to_fsal turns the agent's three answers into the three a client is
 * owed.
 *
 * The split is the reason the protocol carries a status at all: a read past
 * the end of an object, a request that will never be valid and a backend that
 * could not answer this time are different errors to a client, and it acts on
 * each differently.
 */
static fsal_status_t status_to_fsal(enum forebay_status st)
{
	switch (st) {
	case FOREBAY_OK:
		return fsalstat(ERR_FSAL_NO_ERROR, 0);
	case FOREBAY_RANGE:
		/* Past the end of the object. A short read, not an error. */
		return fsalstat(ERR_FSAL_NO_ERROR, 0);
	case FOREBAY_REFUSED:
		return fsalstat(ERR_FSAL_INVAL, 0);
	default:
		/* The agent could not answer. Not ENOENT: the object may well
		 * exist, and telling a client the file is gone would have it
		 * stop asking.
		 */
		return fsalstat(ERR_FSAL_IO, 0);
	}
}

/* handle_new builds a handle for one path.
 *
 * A directory is anything that is not a file, and what makes it a file is the
 * agent answering for its size. Without listing there is nothing else to ask,
 * and treating an unknown path as a directory is what lets a client walk to
 * the shard it is actually after.
 */
static struct forebay_handle *handle_new(struct forebay_export *exp,
					 const char *name, const char *key,
					 bool is_dir, uint64_t size)
{
	struct forebay_handle *h = gsh_calloc(1, sizeof(*h));

	h->export = exp;
	h->name = gsh_strdup(name);
	h->key = key != NULL ? gsh_strdup(key) : NULL;
	h->is_dir = is_dir;
	h->size = size;

	fsal_obj_handle_init(&h->obj_handle, &exp->export,
			     is_dir ? DIRECTORY : REGULAR_FILE);
	h->obj_handle.obj_ops = &forebay_module.handle_ops;
	h->obj_handle.fsid.major = 0;
	h->obj_handle.fsid.minor = 0;
	h->obj_handle.fileid = (uint64_t)(uintptr_t)h;
	return h;
}

static void forebay_release(struct fsal_obj_handle *obj_hdl)
{
	struct forebay_handle *h =
		container_of(obj_hdl, struct forebay_handle, obj_handle);

	if (h == h->export->root)
		return;
	fsal_obj_handle_fini(&h->obj_handle);
	gsh_free(h->name);
	gsh_free(h->key);
	gsh_free(h);
}

/* forebay_lookup resolves one component below a directory.
 *
 * The agent decides. A name it can give a size for is a file; anything else is
 * a directory a client may walk through, because without listing this cannot
 * tell a directory from a name nobody has stored, and refusing would make the
 * whole namespace unreachable.
 */
static fsal_status_t forebay_lookup(struct fsal_obj_handle *parent,
				    const char *name,
				    struct fsal_obj_handle **out,
				    struct fsal_attrlist *attrs)
{
	struct forebay_handle *dir =
		container_of(parent, struct forebay_handle, obj_handle);
	struct forebay_export *exp = dir->export;
	char path[FOREBAY_KEY_MAX + 1];
	char key[FOREBAY_KEY_MAX + 1];
	struct forebay_handle *h;
	enum forebay_path_result pr;
	enum forebay_status st;
	int64_t size = 0;
	int n;

	if (name == NULL || name[0] == '\0')
		return fsalstat(ERR_FSAL_INVAL, 0);

	n = snprintf(path, sizeof(path), "%s/%s",
		     dir->key != NULL ? dir->key : "", name);
	if (n < 0 || (size_t)n >= sizeof(path))
		return fsalstat(ERR_FSAL_NAMETOOLONG, 0);

	pr = forebay_path_key(path, key, sizeof(key));
	switch (pr) {
	case FOREBAY_PATH_OK:
		break;
	case FOREBAY_PATH_TOO_LONG:
		return fsalstat(ERR_FSAL_NAMETOOLONG, 0);
	default:
		/* A name that climbs out of the export, or names nothing, is a
		 * caller asking for what this export does not hold.
		 */
		return fsalstat(ERR_FSAL_NOENT, 0);
	}

	st = forebay_source_size(exp->source, exp->tenant, key, &size);
	if (st == FOREBAY_OK && size >= 0) {
		h = handle_new(exp, name, key, false, (uint64_t)size);
	} else if (st == FOREBAY_FAILED || st == FOREBAY_REFUSED) {
		/* Not a file. Offered as a directory so a client can walk
		 * through it to the object it is actually after, which is the
		 * only way a namespace with no listing is usable at all.
		 */
		h = handle_new(exp, name, key, true, 0);
	} else {
		return status_to_fsal(st);
	}

	*out = &h->obj_handle;
	if (attrs != NULL)
		h->obj_handle.obj_ops->getattrs(&h->obj_handle, attrs);
	return fsalstat(ERR_FSAL_NO_ERROR, 0);
}

static fsal_status_t forebay_getattrs(struct fsal_obj_handle *obj_hdl,
				      struct fsal_attrlist *attrs)
{
	struct forebay_handle *h =
		container_of(obj_hdl, struct forebay_handle, obj_handle);

	attrs->valid_mask = ATTRS_POSIX;
	attrs->type = h->is_dir ? DIRECTORY : REGULAR_FILE;
	attrs->filesize = h->size;
	attrs->spaceused = h->size;
	attrs->fsid = h->obj_handle.fsid;
	attrs->fileid = h->obj_handle.fileid;
	/* Read-only, and the mode says so rather than the write path refusing
	 * later: a client told it may write and then refused has already
	 * decided what to do with the file.
	 */
	attrs->mode = h->is_dir ? 0555 : 0444;
	attrs->numlinks = 1;
	attrs->owner = 0;
	attrs->group = 0;
	attrs->atime.tv_sec = 0;
	attrs->mtime.tv_sec = 0;
	attrs->ctime.tv_sec = 0;
	attrs->change = 0;
	attrs->rawdev.major = 0;
	attrs->rawdev.minor = 0;
	return fsalstat(ERR_FSAL_NO_ERROR, 0);
}

/* forebay_readdir walks one level, asking the agent for it.
 *
 * A directory here is a prefix that has objects beneath it, since the store
 * has none of its own. Paged, because a dataset may hold more shards than one
 * answer carries, and the cursor is the last name seen rather than an index: a
 * store pages by name and an index would drift if anything were added.
 */
static fsal_status_t forebay_readdir(struct fsal_obj_handle *dir_hdl,
				     fsal_cookie_t *whence, void *dir_state,
				     fsal_readdir_cb cb, attrmask_t attrmask,
				     bool *eof)
{
	struct forebay_handle *dir =
		container_of(dir_hdl, struct forebay_handle, obj_handle);
	struct forebay_export *exp = dir->export;
	char after[FOREBAY_ENTRY_NAME_MAX + 1] = "";
	void *buf;

	(void)whence;
	*eof = false;

	buf = gsh_malloc(FOREBAY_LIST_BYTES);
	for (;;) {
		struct forebay_entry e;
		int64_t got = 0, at = 0;
		enum forebay_status st;
		int names = 0, rc;

		st = forebay_source_list(exp->source, exp->tenant,
					 dir->key != NULL ? dir->key : "",
					 after, FOREBAY_LIST_NAMES, buf,
					 FOREBAY_LIST_BYTES, &got);
		if (st == FOREBAY_REFUSED) {
			/* This backend cannot enumerate. An empty directory is
			 * the truthful answer to a question it cannot answer,
			 * and inventing entries would be worse.
			 */
			break;
		}
		if (st != FOREBAY_OK) {
			gsh_free(buf);
			return status_to_fsal(st);
		}

		while ((rc = forebay_entry_next(buf, got, &at, &e)) == 1) {
			struct fsal_obj_handle *child = NULL;
			struct fsal_attrlist attrs;
			enum fsal_dir_result res;
			char path[FOREBAY_KEY_MAX + 1];
			char key[FOREBAY_KEY_MAX + 1];
			int n;

			names++;
			if (strlen(e.name) >= sizeof(after)) {
				gsh_free(buf);
				return fsalstat(ERR_FSAL_NAMETOOLONG, 0);
			}
			strcpy(after, e.name);

			n = snprintf(path, sizeof(path), "%s/%s",
				     dir->key != NULL ? dir->key : "", e.name);
			if (n < 0 || (size_t)n >= sizeof(path))
				continue;
			if (forebay_path_key(path, key, sizeof(key)) != FOREBAY_PATH_OK)
				continue;

			child = &handle_new(exp, e.name, key, e.dir,
					    (uint64_t)e.bytes)->obj_handle;
			fsal_prepare_attrs(&attrs, attrmask);
			child->obj_ops->getattrs(child, &attrs);
			res = cb(e.name, child, &attrs, dir_state,
				 (fsal_cookie_t)(uintptr_t)child);
			fsal_release_attrs(&attrs);
			if (res >= DIR_TERMINATE) {
				gsh_free(buf);
				return fsalstat(ERR_FSAL_NO_ERROR, 0);
			}
		}
		if (rc < 0) {
			/* A reply that ended inside a record is a far side that
			 * does not speak this, not an empty directory.
			 */
			gsh_free(buf);
			return fsalstat(ERR_FSAL_SERVERFAULT, 0);
		}
		if (names < FOREBAY_LIST_NAMES)
			break;
	}
	gsh_free(buf);
	*eof = true;
	return fsalstat(ERR_FSAL_NO_ERROR, 0);
}

static void forebay_read2(struct fsal_obj_handle *obj_hdl, bool bypass,
			  fsal_async_cb done_cb, struct fsal_io_arg *arg,
			  void *caller_arg)
{
	struct forebay_handle *h =
		container_of(obj_hdl, struct forebay_handle, obj_handle);
	fsal_status_t st = fsalstat(ERR_FSAL_NO_ERROR, 0);
	uint64_t offset = arg->offset;

	(void)bypass;
	arg->io_amount = 0;

	if (h->is_dir) {
		done_cb(obj_hdl, fsalstat(ERR_FSAL_ISDIR, 0), arg, caller_arg);
		return;
	}

	for (size_t i = 0; i < arg->iov_count; i++) {
		enum forebay_status fst;
		int64_t got = 0;

		if (arg->iov[i].iov_len == 0)
			continue;
		fst = forebay_source_read(h->export->source, h->export->tenant,
					  h->key, (int64_t)offset,
					  (int64_t)arg->iov[i].iov_len,
					  arg->iov[i].iov_base, &got);
		if (fst == FOREBAY_RANGE) {
			/* The read reached the end of the object. What has
			 * been gathered so far is the answer.
			 */
			arg->end_of_file = true;
			break;
		}
		if (fst != FOREBAY_OK) {
			st = status_to_fsal(fst);
			break;
		}
		arg->io_amount += (size_t)got;
		offset += (uint64_t)got;
		if ((size_t)got < arg->iov[i].iov_len) {
			arg->end_of_file = true;
			break;
		}
	}
	if (offset >= h->size)
		arg->end_of_file = true;
	done_cb(obj_hdl, st, arg, caller_arg);
}

/* forebay_open2 has nothing to open.
 *
 * There is no descriptor behind a file here: a read carries its own offset to
 * the agent, so open is bookkeeping Ganesha needs and this does not. A write
 * is refused rather than silently ignored.
 */
static fsal_status_t forebay_open2(struct fsal_obj_handle *obj_hdl,
				   struct state_t *state,
				   fsal_openflags_t openflags,
				   enum fsal_create_mode createmode,
				   const char *name,
				   struct fsal_attrlist *attrs_in,
				   fsal_verifier_t verifier,
				   struct fsal_obj_handle **new_obj,
				   struct fsal_attrlist *attrs_out,
				   bool *caller_perm_check)
{
	(void)state;
	(void)createmode;
	(void)attrs_in;
	(void)verifier;

	if (openflags & (FSAL_O_WRITE | FSAL_O_RDWR))
		return fsalstat(ERR_FSAL_ROFS, 0);
	if (name != NULL)
		return forebay_lookup(obj_hdl, name, new_obj, attrs_out);
	if (attrs_out != NULL)
		obj_hdl->obj_ops->getattrs(obj_hdl, attrs_out);
	if (caller_perm_check != NULL)
		*caller_perm_check = false;
	return fsalstat(ERR_FSAL_NO_ERROR, 0);
}

/* Named apart from the client's forebay_close, which ends a conversation with
 * the agent. Two functions with one name in one translation unit is a clash
 * the compiler catches; two with names a reader confuses is one it does not.
 */
static fsal_status_t forebay_close_file(struct fsal_obj_handle *obj_hdl)
{
	(void)obj_hdl;
	return fsalstat(ERR_FSAL_NO_ERROR, 0);
}

static fsal_openflags_t forebay_status2(struct fsal_obj_handle *obj_hdl,
					struct state_t *state)
{
	(void)obj_hdl;
	(void)state;
	return FSAL_O_READ;
}

static void forebay_handle_ops_init(struct fsal_obj_ops *ops)
{
	fsal_default_obj_ops_init(ops);
	ops->release = forebay_release;
	ops->lookup = forebay_lookup;
	ops->readdir = forebay_readdir;
	ops->getattrs = forebay_getattrs;
	ops->open2 = forebay_open2;
	ops->status2 = forebay_status2;
	ops->read2 = forebay_read2;
	ops->close = forebay_close_file;
}

/* Export configuration. A socket to ask and a tenant to ask as, which is the
 * whole of it: everything else about what this export holds is the agent's.
 */
struct forebay_export_config {
	char *socket;
	char *tenant;
	int64_t max_reply;
	uint32_t timeout_ms;
	uint32_t retry_ms;
};

static struct config_item forebay_export_params[] = {
	CONF_ITEM_STR("Socket", 1, MAXPATHLEN, NULL,
		      forebay_export_config, socket),
	CONF_ITEM_STR("Tenant", 1, 255, NULL,
		      forebay_export_config, tenant),
	CONF_ITEM_I64("Max_Reply", 4096, INT64_MAX, 8 << 20,
		      forebay_export_config, max_reply),
	CONF_ITEM_UI32("Timeout_Ms", 100, 600000, 30000,
		       forebay_export_config, timeout_ms),
	CONF_ITEM_UI32("Retry_Ms", 0, 600000, 2000,
		       forebay_export_config, retry_ms),
	CONFIG_EOL
};

static struct config_block forebay_export_block = {
	.dbus_interface_name = "org.ganesha.nfsd.config.fsal.forebay-export%d",
	.blk_desc.name = "FSAL",
	.blk_desc.type = CONFIG_BLOCK,
	.blk_desc.u.blk.init = noop_conf_init,
	.blk_desc.u.blk.params = forebay_export_params,
	.blk_desc.u.blk.commit = noop_conf_commit
};

static void forebay_export_release(struct fsal_export *exp_hdl)
{
	struct forebay_export *exp =
		container_of(exp_hdl, struct forebay_export, export);

	if (exp->root != NULL) {
		fsal_obj_handle_fini(&exp->root->obj_handle);
		gsh_free(exp->root->name);
		gsh_free(exp->root->key);
		gsh_free(exp->root);
	}
	forebay_source_free(exp->source);
	gsh_free(exp->tenant);
	fsal_detach_export(exp_hdl->fsal, &exp_hdl->exports);
	free_export_ops(exp_hdl);
	gsh_free(exp);
}

static fsal_status_t forebay_lookup_path(struct fsal_export *exp_hdl,
					 const char *path,
					 struct fsal_obj_handle **out,
					 struct fsal_attrlist *attrs)
{
	struct forebay_export *exp =
		container_of(exp_hdl, struct forebay_export, export);

	(void)path;
	*out = &exp->root->obj_handle;
	if (attrs != NULL)
		exp->root->obj_handle.obj_ops->getattrs(&exp->root->obj_handle, attrs);
	return fsalstat(ERR_FSAL_NO_ERROR, 0);
}

static fsal_status_t forebay_create_export(struct fsal_module *fsal_hdl,
					   void *parse_node,
					   struct config_error_type *err_type,
					   const struct fsal_up_vector *up_ops)
{
	struct forebay_export_config cfg = {
		.max_reply = 8 << 20, .timeout_ms = 30000, .retry_ms = 2000,
	};
	struct forebay_export *exp;
	int rc;

	rc = load_config_from_node(parse_node, &forebay_export_block, &cfg,
				   true, err_type);
	if (rc != 0)
		return fsalstat(ERR_FSAL_INVAL, 0);
	if (cfg.socket == NULL || cfg.tenant == NULL) {
		/* Refused rather than defaulted. A tenant guessed here is
		 * another tenant's bytes, and a socket guessed is an export
		 * that serves nothing and says nothing about why.
		 */
		LogCrit(COMPONENT_FSAL,
			"forebay: an export needs both Socket and Tenant");
		return fsalstat(ERR_FSAL_INVAL, 0);
	}

	exp = gsh_calloc(1, sizeof(*exp));
	fsal_export_init(&exp->export);
	exp->export.exp_ops.lookup_path = forebay_lookup_path;
	exp->export.exp_ops.release = forebay_export_release;
	exp->export.fsal = fsal_hdl;
	exp->export.up_ops = up_ops;
	exp->tenant = gsh_strdup(cfg.tenant);

	/* Not dialled here. An export configured before the agent is running is
	 * ordinary on a node, and failing to load for it would make the order
	 * they start in matter.
	 */
	exp->source = forebay_source_new(cfg.socket, cfg.max_reply,
					 (int)cfg.timeout_ms,
					 (int64_t)cfg.retry_ms);
	if (exp->source == NULL) {
		gsh_free(exp->tenant);
		gsh_free(exp);
		return fsalstat(ERR_FSAL_INVAL, 0);
	}

	exp->root = handle_new(exp, "/", NULL, true, 0);
	if (fsal_attach_export(fsal_hdl, &exp->export.exports) != 0) {
		forebay_export_release(&exp->export);
		return fsalstat(ERR_FSAL_SERVERFAULT, 0);
	}
	LogEvent(COMPONENT_FSAL, "forebay: export for tenant %s reading %s",
		 cfg.tenant, cfg.socket);
	return fsalstat(ERR_FSAL_NO_ERROR, 0);
}

static fsal_status_t forebay_init_config(struct fsal_module *fsal_hdl,
					 config_file_t config_struct,
					 struct config_error_type *err_type)
{
	(void)config_struct;
	(void)err_type;
	fsal_hdl->fs_info = forebay_info;
	return fsalstat(ERR_FSAL_NO_ERROR, 0);
}

MODULE_INIT void forebay_init(void)
{
	struct fsal_module *m = &forebay_module.fsal;

	if (register_fsal(m, FOREBAY_NAME, FSAL_MAJOR_VERSION,
			  FSAL_MINOR_VERSION, FSAL_ID_NO_PNFS) != 0) {
		LogCrit(COMPONENT_FSAL, "forebay: could not register");
		return;
	}
	forebay_module.fsal.m_ops.create_export = forebay_create_export;
	forebay_module.fsal.m_ops.init_config = forebay_init_config;
	forebay_handle_ops_init(&forebay_module.handle_ops);
}

MODULE_FINI void forebay_unload(void)
{
	if (unregister_fsal(&forebay_module.fsal) != 0)
		LogCrit(COMPONENT_FSAL, "forebay: could not unregister");
}
